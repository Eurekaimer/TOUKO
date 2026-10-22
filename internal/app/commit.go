package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

type CommitResult struct {
	Head        domain.RepositoryHead `json:"head"`
	ChangeCount int                   `json:"change_count"`
	AssetCount  int                   `json:"asset_count"`
	Cloudflare  *CloudflareResult     `json:"cloudflare,omitempty"`
}

func stagedChanges(st index.State) domain.ChangeSet {
	changes := make([]domain.Change, 0, len(st.Changes))
	for _, c := range st.Changes {
		changes = append(changes, c)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].AssetID.String() < changes[j].AssetID.String() })
	return domain.ChangeSet{BaseHead: st.BaseHead, Changes: changes}
}

func (s *Service) Commit(ctx context.Context, r repository.Git, message string) (CommitResult, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	count := len(st.Changes)
	if st.Publication != nil {
		result, err := s.recoverPublication(ctx, r, st, true)
		return CommitResult{Head: result.Head, ChangeCount: count, AssetCount: result.AssetCount, Cloudflare: result.Cloudflare}, err
	}
	if len(st.Changes) == 0 {
		result, err := s.syncLocked(ctx, r)
		return CommitResult{Head: result.Head, AssetCount: result.AssetCount, Cloudflare: result.Cloudflare}, err
	}
	if err = mutable(st); err != nil {
		return CommitResult{}, err
	}
	head, err := r.Head(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	if head != st.BaseHead {
		remote, err := repository.ReadSnapshot(ctx, r, head)
		if err != nil {
			return CommitResult{}, err
		}
		if err = sameRepository(st, remote); err != nil {
			return CommitResult{}, err
		}
		for _, c := range stagedChanges(st).Changes {
			if c.Kind == domain.Update {
				if _, exists := remote.Assets[c.AssetID]; !exists {
					id := c.AssetID
					return CommitResult{}, &domain.ConflictError{Base: st.BaseHead, Remote: head, AssetID: &id, Reason: "delete_modify"}
				}
			}
		}
		return CommitResult{}, &domain.ConflictError{Base: st.BaseHead, Remote: head, Reason: "head_changed"}
	}
	base, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		return CommitResult{}, err
	}
	if err = sameRepository(st, base); err != nil {
		return CommitResult{}, err
	}
	plan, err := repository.PlanCommit(base, stagedChanges(st))
	if err != nil {
		return CommitResult{}, err
	}
	if plan.Empty() {
		return CommitResult{Head: head, AssetCount: len(base.Assets)}, nil
	}
	changes := make([]repository.TreeChange, 0, len(plan.NewObjects)+len(plan.MetadataWrites)+len(plan.Deletions))
	for _, p := range plan.ReusedObjects {
		if err = repository.VerifyObject(ctx, r, p.Blob, p.Object); err != nil {
			return CommitResult{}, err
		}
	}
	for _, p := range plan.NewObjects {
		blob, err := s.uploadStaged(ctx, r, p)
		if err != nil {
			return CommitResult{}, err
		}
		changes = append(changes, repository.TreeChange{Path: p.Path, Blob: &blob})
	}
	for _, write := range plan.MetadataWrites {
		blob, err := r.CreateBlob(ctx, bytes.NewReader(write.Data), int64(len(write.Data)))
		if err != nil {
			return CommitResult{}, err
		}
		changes = append(changes, repository.TreeChange{Path: write.Path, Blob: &blob})
	}
	for _, path := range plan.Deletions {
		changes = append(changes, repository.TreeChange{Path: path})
	}
	tree, err := r.CreateTree(ctx, base.Tree, changes)
	if err != nil {
		return CommitResult{}, err
	}
	candidate, err := r.CreateCommit(ctx, tree, base.Head, message)
	if err != nil {
		return CommitResult{}, err
	}
	st.Publication = &index.Publication{BaseHead: head, CandidateHead: candidate, CandidateTree: string(tree)}
	if err = s.Store.SavePublication(ctx, st.Publication); err != nil {
		return CommitResult{}, err
	}
	result, err := s.publishJournal(ctx, r, st)
	return CommitResult{Head: result.Head, ChangeCount: count, AssetCount: result.AssetCount, Cloudflare: result.Cloudflare}, err
}

type stagedReader struct {
	ctx    context.Context
	source io.Reader
	digest hash.Hash
	size   int64
}

func (r *stagedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p)
	if n > 0 {
		_, _ = r.digest.Write(p[:n])
		r.size += int64(n)
	}
	return n, err
}
func (s *Service) uploadStaged(ctx context.Context, r repository.Writer, p repository.ObjectPlacement) (repository.BlobID, error) {
	file, err := os.Open(staging.PayloadPath(s.Dir, p.Namespace, p.Object))
	if err != nil {
		return "", err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return "", err
	}
	if info.Size() != p.Object.Size {
		_ = file.Close()
		return "", fmt.Errorf("%w: staged object size %s", domain.ErrHashMismatch, p.Path)
	}
	reader := &stagedReader{ctx: ctx, source: io.LimitReader(file, p.Object.Size+1), digest: sha256.New()}
	blob, err := r.CreateBlob(ctx, reader, p.Object.Size)
	closeErr := file.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if reader.size != p.Object.Size || !bytes.Equal(reader.digest.Sum(nil), p.Object.Hash[:]) {
		return "", fmt.Errorf("%w: staged object %s", domain.ErrHashMismatch, p.Path)
	}
	return blob, nil
}

// publicationRelation only treats a successful immutable ancestry query as proof
// of publication. A failed read must never discard the recovery journal.
func publicationRelation(ctx context.Context, r repository.Reader, p *index.Publication) (domain.RepositoryHead, bool, error) {
	head, err := r.Head(ctx)
	if err != nil {
		return "", false, err
	}
	if head == p.CandidateHead {
		return head, true, nil
	}
	if head == p.BaseHead {
		return head, false, nil
	}
	if head == "" {
		return head, false, nil
	}
	published, err := r.IsAncestor(ctx, p.CandidateHead, head)
	return head, published, err
}
func unknownPublication(p *index.Publication, err error) error {
	return fmt.Errorf("publication outcome unknown for candidate %s; run sync: %w", p.CandidateHead, err)
}
func (s *Service) recoverPublication(ctx context.Context, r repository.Git, st index.State, retry bool) (SyncResult, error) {
	head, published, err := publicationRelation(ctx, r, st.Publication)
	if err != nil {
		return SyncResult{}, unknownPublication(st.Publication, err)
	}
	if published {
		return s.finishPublication(ctx, r, st)
	}
	if head != st.Publication.BaseHead {
		return SyncResult{}, &domain.ConflictError{Base: st.Publication.BaseHead, Remote: head, Reason: "head_changed"}
	}
	if retry {
		return s.publishJournal(ctx, r, st)
	}
	// Sync may abandon an unpublished candidate, but never the staged batch.
	if err = s.Store.SavePublication(ctx, nil); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Head: st.Head, AssetCount: len(st.Assets)}, nil
}
func (s *Service) publishJournal(ctx context.Context, r repository.Git, st index.State) (SyncResult, error) {
	candidate, err := validateCandidate(ctx, r, st)
	if err != nil {
		return SyncResult{}, err
	}
	p := st.Publication
	head, err := r.Head(ctx)
	if err != nil {
		return SyncResult{}, unknownPublication(p, err)
	}
	if head != p.BaseHead {
		return s.recoverPublication(ctx, r, st, false)
	}
	if err = r.UpdateRef(ctx, p.BaseHead, p.CandidateHead); err != nil {
		head, published, readErr := publicationRelation(ctx, r, p)
		if readErr != nil {
			return SyncResult{}, unknownPublication(p, readErr)
		}
		if published {
			return s.finalizeCandidate(ctx, r, st, candidate)
		}
		if head != p.BaseHead {
			return SyncResult{}, &domain.ConflictError{Base: p.BaseHead, Remote: head, Reason: "head_changed"}
		}
		return SyncResult{}, err
	}
	return s.finalizeCandidate(ctx, r, st, candidate)
}
func (s *Service) finishPublication(ctx context.Context, r repository.Git, st index.State) (SyncResult, error) {
	candidate, err := validateCandidate(ctx, r, st)
	if err != nil {
		return SyncResult{}, err
	}
	return s.finalizeCandidate(ctx, r, st, candidate)
}
func (s *Service) finalizeCandidate(ctx context.Context, r repository.Git, st index.State, candidate repository.Snapshot) (SyncResult, error) {
	head, published, err := publicationRelation(ctx, r, st.Publication)
	if err != nil {
		return SyncResult{}, unknownPublication(st.Publication, err)
	}
	if !published {
		return SyncResult{}, &domain.ConflictError{Base: st.Publication.BaseHead, Remote: head, Reason: "head_changed"}
	}
	snap := candidate
	if head != candidate.Head {
		snap, err = repository.ReadSnapshot(ctx, r, head)
		if err != nil {
			return SyncResult{}, err
		}
		if snap.Manifest.ID != candidate.Manifest.ID {
			return SyncResult{}, fmt.Errorf("%w: descendant repository identity differs", domain.ErrConflict)
		}
	}
	if err = sameRepository(st, snap); err != nil {
		return SyncResult{}, err
	}
	result := SyncResult{Head: snap.Head, AssetCount: len(snap.Assets)}
	next := snapshotState(st, snap, true)
	if err = s.Store.Save(ctx, next); err != nil {
		return result, fmt.Errorf("published head %s; local index update failed; run sync: %w", head, err)
	}
	result.Cloudflare = s.cloudflarePublication(ctx, r, st, snap)
	if err = s.cleanup(ctx, next); err != nil {
		return result, fmt.Errorf("published head %s; local payload cleanup failed: %w", head, err)
	}
	return result, nil
}

func candidateCorrupt(reason string) error {
	return fmt.Errorf("%w: publication candidate %s", domain.ErrRepositoryCorrupt, reason)
}

// Rebuild the intended batch from durable staging and the immutable base. Check
// the entire tree delta, not just gallery counts: deleted binaries and metadata
// must be absent and unrelated files must retain their exact Git entries.
func validateCandidate(ctx context.Context, r repository.Reader, st index.State) (repository.Snapshot, error) {
	p := st.Publication
	snap, err := repository.ReadSnapshot(ctx, r, p.CandidateHead)
	if err != nil {
		return snap, err
	}
	if string(snap.Tree) != p.CandidateTree {
		return snap, candidateCorrupt("tree differs from journal")
	}
	commit, err := r.Commit(ctx, p.CandidateHead)
	if err != nil {
		return snap, err
	}
	if (p.BaseHead == "" && len(commit.Parents) != 0) || (p.BaseHead != "" && (len(commit.Parents) != 1 || commit.Parents[0] != p.BaseHead)) {
		return snap, candidateCorrupt("has unexpected parent")
	}
	if err = sameRepository(st, snap); err != nil {
		return snap, err
	}
	var baseEntries []repository.GitEntry
	var writes []repository.MetadataWrite
	var deletions []string
	var objects []repository.ObjectPlacement
	if st.Manifest == nil && len(st.Changes) == 0 {
		if len(snap.Assets) != 0 {
			return snap, candidateCorrupt("initialization contains assets")
		}
		if p.BaseHead != "" {
			c, e := r.Commit(ctx, p.BaseHead)
			if e != nil {
				return snap, e
			}
			baseEntries, e = r.Tree(ctx, c.Tree)
			if e != nil {
				return snap, e
			}
		}
		data, e := repository.EncodeManifest(snap.Manifest)
		if e != nil {
			return snap, e
		}
		writes = []repository.MetadataWrite{{Path: repository.ManifestPath, Data: data}}
		for _, entry := range baseEntries {
			if reservedPath(entry.Path) {
				return snap, candidateCorrupt("initialization occupied reserved paths")
			}
		}
	} else {
		if st.BaseHead != p.BaseHead {
			return snap, candidateCorrupt("staging base differs from journal")
		}
		base, e := repository.ReadSnapshot(ctx, r, p.BaseHead)
		if e != nil {
			return snap, e
		}
		if e = sameRepository(st, base); e != nil {
			return snap, e
		}
		plan, e := repository.PlanCommit(base, stagedChanges(st))
		if e != nil {
			return snap, e
		}
		if len(plan.FinalAssets) != len(snap.Assets) {
			return snap, candidateCorrupt("asset set differs from staged batch")
		}
		for id, a := range plan.FinalAssets {
			b, ok := snap.Assets[id]
			if !ok {
				return snap, candidateCorrupt("missing staged asset")
			}
			want, _ := repository.EncodeAsset(a)
			got, _ := repository.EncodeAsset(b)
			if !bytes.Equal(want, got) {
				return snap, candidateCorrupt("metadata differs from staged batch")
			}
		}
		baseEntries = base.Entries
		writes = plan.MetadataWrites
		deletions = plan.Deletions
		objects = append(plan.NewObjects, plan.ReusedObjects...)
	}
	expected := make(map[string]repository.GitEntry, len(baseEntries)+len(writes)+len(objects))
	actual := make(map[string]repository.GitEntry, len(snap.Entries))
	// Directory entries are derived from their descendants; changing any leaf
	// necessarily changes ancestor tree IDs. Only leaves are publication data.
	for _, e := range baseEntries {
		if e.Type != "tree" {
			expected[e.Path] = e
		}
	}
	for _, e := range snap.Entries {
		if e.Type != "tree" {
			actual[e.Path] = e
		}
	}
	for _, path := range deletions {
		delete(expected, path)
	}
	for _, write := range writes {
		entry, ok := actual[write.Path]
		if !ok || entry.Type != "blob" || entry.Mode != "100644" || entry.Size != int64(len(write.Data)) {
			return snap, candidateCorrupt("metadata write missing")
		}
		body, e := r.OpenBlob(ctx, repository.BlobID(entry.SHA))
		if e != nil {
			return snap, e
		}
		data, e := io.ReadAll(io.LimitReader(body, int64(len(write.Data))+1))
		closeErr := body.Close()
		if e != nil {
			return snap, e
		}
		if closeErr != nil {
			return snap, closeErr
		}
		if !bytes.Equal(data, write.Data) {
			return snap, candidateCorrupt("metadata bytes differ")
		}
		expected[write.Path] = entry
	}
	for _, object := range objects {
		entry, ok := actual[object.Path]
		if !ok || entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || entry.Size != object.Object.Size {
			return snap, candidateCorrupt("object placement missing")
		}
		if object.Blob != "" && string(object.Blob) != entry.SHA {
			return snap, candidateCorrupt("reused object replaced")
		}
		if e := repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), object.Object); e != nil {
			return snap, e
		}
		expected[object.Path] = entry
	}
	if len(expected) != len(actual) {
		return snap, candidateCorrupt("contains unexpected or missing paths")
	}
	for path, want := range expected {
		if got, ok := actual[path]; !ok || got != want {
			return snap, candidateCorrupt("changed unrelated path " + path)
		}
	}
	return snap, nil
}
