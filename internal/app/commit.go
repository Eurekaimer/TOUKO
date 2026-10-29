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
	// The staged base was captured when the first change was made. The adapter
	// reads the branch before PATCHing it, so a rejected update still needs the
	// authoritative relationship query: the read/PATCH pair is not an atomic
	// compare-and-swap and cannot prove the expected old head under concurrency.
	head := st.BaseHead
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
	blobs := make(map[string]repository.BlobID, len(plan.NewObjects)+len(plan.MetadataWrites))
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
		blobs[p.Path] = blob
		changes = append(changes, repository.TreeChange{Path: p.Path, Blob: &blob})
	}
	for _, write := range plan.MetadataWrites {
		blob, err := r.CreateBlob(ctx, bytes.NewReader(write.Data), int64(len(write.Data)))
		if err != nil {
			return CommitResult{}, err
		}
		blobs[write.Path] = blob
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
	// The tree and commit were just accepted with the exact entries and parent
	// we sent, so the local state comes from the plan instead of re-reading it.
	snap, err := plannedSnapshot(base, plan, blobs, candidate, tree)
	if err != nil {
		return CommitResult{}, err
	}
	st.Publication = &index.Publication{BaseHead: head, CandidateHead: candidate, CandidateTree: string(tree)}
	if err = s.Store.SavePublication(ctx, st.Publication); err != nil {
		return CommitResult{}, err
	}
	result, err := s.publish(ctx, r, st, snap)
	return CommitResult{Head: result.Head, ChangeCount: count, AssetCount: result.AssetCount, Cloudflare: result.Cloudflare}, err
}

// plannedSnapshot is the local view of what the accepted writes produce. The
// authoritative read happens at the start of every commit, so a wrong entry
// here cannot corrupt a later plan; recovery still re-reads and validates.
func plannedSnapshot(base repository.Snapshot, plan repository.CommitPlan, blobs map[string]repository.BlobID, head domain.RepositoryHead, tree repository.TreeID) (repository.Snapshot, error) {
	snap := repository.Snapshot{Head: head, Tree: tree, Assets: plan.FinalAssets, Manifest: base.Manifest}
	entries := make(map[string]repository.GitEntry, len(base.Entries)+len(plan.NewObjects)+len(plan.MetadataWrites))
	for _, entry := range base.Entries {
		entries[entry.Path] = entry
	}
	for _, path := range plan.Deletions {
		delete(entries, path)
	}
	for _, p := range plan.NewObjects {
		blob, ok := blobs[p.Path]
		if !ok {
			return repository.Snapshot{}, fmt.Errorf("planned object %s has no blob", p.Path)
		}
		entries[p.Path] = repository.GitEntry{Path: p.Path, Mode: "100644", Type: "blob", SHA: string(blob), Size: p.Object.Size}
	}
	for _, write := range plan.MetadataWrites {
		blob, ok := blobs[write.Path]
		if !ok {
			return repository.Snapshot{}, fmt.Errorf("planned write %s has no blob", write.Path)
		}
		entries[write.Path] = repository.GitEntry{Path: write.Path, Mode: "100644", Type: "blob", SHA: string(blob), Size: int64(len(write.Data))}
		if write.Path == repository.ManifestPath {
			manifest, err := repository.DecodeManifest(write.Data)
			if err != nil {
				return repository.Snapshot{}, err
			}
			snap.Manifest = manifest
		}
	}
	// Cheap local check: every asset the plan publishes must have a real blob
	// entry. This catches a wrong construction without reading the server back.
	for _, asset := range snap.Assets {
		entry, ok := entries[repository.ObjectPath(asset)]
		if !ok || entry.Path == "" || entry.Type != "blob" || entry.Size != asset.Object.Size {
			return repository.Snapshot{}, fmt.Errorf("planned entry for %s is inconsistent", repository.ObjectPath(asset))
		}
	}
	snap.Entries = make([]repository.GitEntry, 0, len(entries))
	for _, entry := range entries {
		snap.Entries = append(snap.Entries, entry)
	}
	return snap, nil
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
		if r.digest != nil {
			_, _ = r.digest.Write(p[:n])
		}
		r.size += int64(n)
	}
	return n, err
}
func (s *Service) uploadStaged(ctx context.Context, r repository.Writer, p repository.ObjectPlacement) (repository.BlobID, error) {
	file, err := os.Open(staging.PayloadPathFor(s.Dir, p.Namespace, p.Object))
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
	if !p.Object.HasHash() {
		_ = file.Close()
		return "", fmt.Errorf("%w: staged object %s has no content hash", domain.ErrHashMismatch, p.Path)
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
		return s.finalizePublished(ctx, r, st, head)
	}
	if head != st.Publication.BaseHead {
		return SyncResult{}, &domain.ConflictError{Base: st.Publication.BaseHead, Remote: head, Reason: "head_changed"}
	}
	if retry {
		return s.publishJournal(ctx, r, st, nil)
	}
	// Sync may abandon an unpublished candidate, but never the staged batch.
	if err = s.Store.SavePublication(ctx, nil); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Head: st.Head, AssetCount: len(st.Assets)}, nil
}
func (s *Service) publishJournal(ctx context.Context, r repository.Git, st index.State, base *repository.Snapshot) (SyncResult, error) {
	if st.Publication != nil && st.Publication.Operation != "" {
		return s.publishOperationJournal(ctx, r, st, base)
	}
	candidate, err := validateCandidate(ctx, r, st, base)
	if err != nil {
		return SyncResult{}, err
	}
	return s.publish(ctx, r, st, candidate)
}

// publish updates the branch. The adapter reads the branch before PATCHing, so
// rejection is diagnosed with the authoritative relationship query rather than
// assumed to be a non-fast-forward alone.
func (s *Service) publish(ctx context.Context, r repository.Git, st index.State, candidate repository.Snapshot) (SyncResult, error) {
	p := st.Publication
	if err := r.UpdateRef(ctx, p.BaseHead, p.CandidateHead); err != nil {
		head, published, readErr := publicationRelation(ctx, r, p)
		if readErr != nil {
			return SyncResult{}, unknownPublication(p, readErr)
		}
		if published {
			snap, err := currentSnapshot(ctx, r, candidate, head)
			if err != nil {
				return SyncResult{}, err
			}
			return s.finalizeSnapshot(ctx, r, st, snap)
		}
		if head != p.BaseHead {
			return SyncResult{}, staleConflict(ctx, r, st, head)
		}
		return SyncResult{}, err
	}
	return s.finalizeSnapshot(ctx, r, st, candidate)
}

// staleConflict explains a rejected ref update: an edited Asset that the remote
// deleted is delete_modify, everything else is head_changed.
func staleConflict(ctx context.Context, r repository.Reader, st index.State, head domain.RepositoryHead) error {
	base := st.Publication.BaseHead
	if snap, err := repository.ReadSnapshot(ctx, r, head); err == nil {
		for _, c := range stagedChanges(st).Changes {
			if c.Kind == domain.Update {
				if _, exists := snap.Assets[c.AssetID]; !exists {
					id := c.AssetID
					return &domain.ConflictError{Base: base, Remote: head, AssetID: &id, Reason: "delete_modify"}
				}
			}
		}
	}
	return &domain.ConflictError{Base: base, Remote: head, Reason: "head_changed"}
}

// finalizePublished validates a journal whose candidate is already published
// and records the head that is current, so a descendant is never mistaken for
// the candidate.
func (s *Service) finalizePublished(ctx context.Context, r repository.Git, st index.State, head domain.RepositoryHead) (SyncResult, error) {
	if st.Publication != nil && st.Publication.Operation != "" {
		return s.finalizeOperationPublished(ctx, r, st, head)
	}
	candidate, err := validateCandidate(ctx, r, st, nil)
	if err != nil {
		return SyncResult{}, err
	}
	snap, err := currentSnapshot(ctx, r, candidate, head)
	if err != nil {
		return SyncResult{}, err
	}
	return s.finalizeSnapshot(ctx, r, st, snap)
}

// currentSnapshot follows a confirmed published candidate to the head that is
// actually current, so a descendant is recorded instead of the candidate.
func currentSnapshot(ctx context.Context, r repository.Reader, candidate repository.Snapshot, head domain.RepositoryHead) (repository.Snapshot, error) {
	if head == candidate.Head {
		return candidate, nil
	}
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		return repository.Snapshot{}, err
	}
	if snap.Manifest.ID != candidate.Manifest.ID {
		return repository.Snapshot{}, fmt.Errorf("%w: descendant repository identity differs", domain.ErrConflict)
	}
	return snap, nil
}
func (s *Service) finalizeSnapshot(ctx context.Context, r repository.Git, st index.State, snap repository.Snapshot) (SyncResult, error) {
	if err := sameRepository(st, snap); err != nil {
		return SyncResult{}, err
	}
	head := snap.Head
	result := SyncResult{Head: head, AssetCount: len(snap.Assets)}
	next := snapshotState(st, snap, true)
	if err := s.Store.Save(ctx, next); err != nil {
		return result, fmt.Errorf("published head %s; local index update failed; run sync: %w", head, err)
	}
	if st.Publication == nil || st.Publication.Operation == "" {
		result.Cloudflare = s.cloudflarePublication(ctx, r, st, snap)
	}
	if err := s.cleanup(ctx, next); err != nil {
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
func validateCandidate(ctx context.Context, r repository.Reader, st index.State, trustedBase *repository.Snapshot) (repository.Snapshot, error) {
	p := st.Publication
	if trustedBase != nil && trustedBase.Head != p.BaseHead {
		return repository.Snapshot{}, candidateCorrupt("trusted base differs from journal")
	}
	var snap repository.Snapshot
	var err error
	if trustedBase != nil {
		snap, err = repository.ReadSnapshotReusing(ctx, r, p.CandidateHead, *trustedBase)
	} else {
		snap, err = repository.ReadSnapshot(ctx, r, p.CandidateHead)
	}
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
		var base repository.Snapshot
		var e error
		if trustedBase != nil {
			base = *trustedBase
		} else {
			base, e = repository.ReadSnapshot(ctx, r, p.BaseHead)
			if e != nil {
				return snap, e
			}
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
