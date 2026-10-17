package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/image"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

// DefaultCapacityBytes is the per-repository publication waterline. It leaves
// headroom below jsDelivr's 150 MB package limit for the rest of the repository.
const DefaultCapacityBytes int64 = 140_000_000

// ErrCapacityFull reports that a prepared batch would push the projected
// repository tree past the caller's byte limit.
var ErrCapacityFull = errors.New("capacity full")

// PreparedInput pairs immutable asset metadata with the ingested payload it
// describes. PrepareInput creates the identity exactly once; callers may plan
// and route the batch before handing every accepted input to AddPrepared.
//
// The payload temporary is owned by the caller until AddPrepared installs it.
// A failed AddPrepared leaves every payload temporary untouched so routing can
// still proceed; staging.Install consumes a temporary once it processes it.
type PreparedInput struct {
	Asset   domain.Asset
	Payload image.Result
}

// PreparedOptions configures AddPrepared. Reader is required: capacity and
// staleness are always checked against the actual repository HEAD under the
// service lock. MaxBytes <= 0 selects DefaultCapacityBytes.
type PreparedOptions struct {
	Reader   repository.Reader
	MaxBytes int64
}

// Capacity describes the repository's full indexed HEAD tree plus a proposed
// staged batch. UsedBytes counts every blob in HEAD, including files TOUKO does
// not manage; ProjectedBytes applies the exact CommitPlan for the batch.
type Capacity struct {
	Head           domain.RepositoryHead `json:"head"`
	UsedBytes      int64                 `json:"used_bytes"`
	ProjectedBytes int64                 `json:"projected_bytes"`
	LimitBytes     int64                 `json:"limit_bytes"`
	RemainingBytes int64                 `json:"remaining_bytes"`
}

// CapacityError carries the rejected projection. It unwraps to
// ErrCapacityFull so callers can report per-member remaining capacity.
type CapacityError struct {
	Capacity Capacity
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("%s: projected %d bytes exceeds %d byte limit", ErrCapacityFull, e.Capacity.ProjectedBytes, e.Capacity.LimitBytes)
}
func (e *CapacityError) Unwrap() error { return ErrCapacityFull }

func capacityLimit(max int64) (int64, error) {
	if max < 0 {
		return 0, fmt.Errorf("invalid capacity limit %d", max)
	}
	if max == 0 {
		return DefaultCapacityBytes, nil
	}
	return max, nil
}

func optionNamespace(opts AddOptions) (domain.Namespace, error) {
	ns, err := domain.ParseNamespace(string(opts.Namespace))
	if err != nil {
		return "", err
	}
	if opts.Directory != "" {
		if err := domain.ValidateRepositoryPath(opts.Directory); err != nil {
			return "", err
		}
	}
	return ns, nil
}

// PrepareInput builds the asset identity and metadata for one ingested payload.
// It performs no staging or repository access, so callers can prepare a batch,
// preview its capacity, and then install it through AddPrepared.
func PrepareInput(result image.Result, name string, opts AddOptions) (PreparedInput, error) {
	if err := result.Object.Validate(); err != nil {
		return PreparedInput{}, err
	}
	ns, err := optionNamespace(opts)
	if err != nil {
		return PreparedInput{}, err
	}
	id, err := domain.NewAssetID()
	if err != nil {
		return PreparedInput{}, err
	}
	original := "stdin." + result.Object.Ext
	if name != "" {
		original = filepath.Base(name)
	}
	title := original
	if opts.Title != nil {
		title = *opts.Title
	}
	now := time.Now().UTC()
	a := domain.Asset{ID: id, Namespace: ns, Object: result.Object, OriginalName: original, Title: title, Tags: normalize(opts.Tags), Collections: normalize(opts.Collections), CreatedAt: now, UpdatedAt: now}
	if opts.Directory != "" {
		a.Path = path.Join(opts.Directory, a.Object.Hash.String()+"."+a.Object.Ext)
	}
	if err := a.Validate(); err != nil {
		return PreparedInput{}, err
	}
	return PreparedInput{Asset: a, Payload: result}, nil
}

func validatePrepared(inputs []PreparedInput, requirePayload bool) error {
	if len(inputs) == 0 {
		return fmt.Errorf("no input images")
	}
	seen := make(map[domain.AssetID]bool, len(inputs))
	for i, input := range inputs {
		a := input.Asset
		if a.ID == (domain.AssetID{}) {
			return fmt.Errorf("input %d: prepared asset has no identity", i)
		}
		if seen[a.ID] {
			return fmt.Errorf("input %d: duplicate prepared asset %s", i, a.ID)
		}
		seen[a.ID] = true
		if a.Object != input.Payload.Object {
			return fmt.Errorf("input %d: prepared metadata describes different bytes than the payload", i)
		}
		if requirePayload && input.Payload.TempPath == "" {
			return fmt.Errorf("input %d: prepared payload has no source file", i)
		}
		if err := a.Validate(); err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
	}
	return nil
}

// applyPrepared copies the staged state and adds one immutable prepared input
// per entry. Path occupancy and identity collisions are rejected here, before
// any payload is installed.
func applyPrepared(st index.State, inputs []PreparedInput) (index.State, error) {
	next := st
	next.Changes = make(map[domain.AssetID]domain.Change, len(st.Changes)+len(inputs))
	for id, c := range st.Changes {
		next.Changes[id] = c
	}
	paths := make(map[string]domain.ContentObject, len(st.Assets)+len(st.Changes)+len(inputs))
	for _, asset := range st.Assets {
		paths[repository.ObjectPath(asset)] = asset.Object
	}
	for _, change := range st.Changes {
		if change.Asset != nil {
			paths[repository.ObjectPath(*change.Asset)] = change.Asset.Object
		}
	}
	for i, input := range inputs {
		a := input.Asset
		if _, exists := st.Assets[a.ID]; exists {
			return index.State{}, fmt.Errorf("%w: input %d asset already exists: %s", domain.ErrConflict, i, a.ID)
		}
		if _, exists := st.Changes[a.ID]; exists {
			return index.State{}, fmt.Errorf("%w: input %d asset already has staged changes: %s", domain.ErrConflict, i, a.ID)
		}
		objectPath := repository.ObjectPath(a)
		if existing, ok := paths[objectPath]; ok && existing != a.Object {
			return index.State{}, fmt.Errorf("occupied image path %s", objectPath)
		}
		paths[objectPath] = a.Object
		asset := a
		next.Changes[a.ID] = domain.Change{Kind: domain.Add, AssetID: a.ID, Asset: &asset}
	}
	bindBase(&next)
	return next, nil
}

// projectCapacity reads the current repository HEAD and computes the full tree
// bytes plus the proposed batch through the real CommitPlan. Missing or stale
// remote state is an error, never a zero-byte projection.
func projectCapacity(ctx context.Context, reader repository.Reader, st index.State, next index.State) (repository.Snapshot, int64, int64, error) {
	if reader == nil {
		return repository.Snapshot{}, 0, 0, fmt.Errorf("capacity requires repository access")
	}
	head, err := reader.Head(ctx)
	if err != nil {
		return repository.Snapshot{}, 0, 0, err
	}
	if head != st.Head {
		return repository.Snapshot{}, 0, 0, &domain.ConflictError{Base: st.Head, Remote: head, Reason: "head_changed"}
	}
	base, err := repository.ReadSnapshot(ctx, reader, head)
	if err != nil {
		return repository.Snapshot{}, 0, 0, err
	}
	if err := sameRepository(st, base); err != nil {
		return repository.Snapshot{}, 0, 0, err
	}
	used, err := repository.BlobBytes(base.Entries)
	if err != nil {
		return repository.Snapshot{}, 0, 0, err
	}
	changes := stagedChanges(next)
	if len(changes.Changes) == 0 {
		return base, used, used, nil
	}
	plan, err := repository.PlanCommit(base, changes)
	if err != nil {
		return repository.Snapshot{}, 0, 0, err
	}
	projected, err := repository.ProjectedBlobBytes(base, plan)
	if err != nil {
		return repository.Snapshot{}, 0, 0, err
	}
	return base, used, projected, nil
}

func capacityFor(base repository.Snapshot, used, projected, limit int64) Capacity {
	remaining := limit - projected
	if remaining < 0 {
		remaining = 0
	}
	return Capacity{Head: base.Head, UsedBytes: used, ProjectedBytes: projected, LimitBytes: limit, RemainingBytes: remaining}
}

// PreviewPrepared reports the proposed capacity for a prepared batch without
// acquiring the service lock or changing durable state. It uses the cached HEAD
// and verifies that HEAD still matches the repository, so callers must treat a
// later AddPrepared check as authoritative.
func (s *Service) PreviewPrepared(ctx context.Context, reader repository.Reader, inputs []PreparedInput, limit int64) (Capacity, error) {
	if reader == nil {
		return Capacity{}, fmt.Errorf("capacity requires repository access")
	}
	lim, err := capacityLimit(limit)
	if err != nil {
		return Capacity{}, err
	}
	st, err := s.Store.Load(ctx)
	if err != nil {
		return Capacity{}, err
	}
	if err := mutable(st); err != nil {
		return Capacity{}, err
	}
	if len(inputs) > 0 {
		if err := validatePrepared(inputs, false); err != nil {
			return Capacity{}, err
		}
	}
	next, err := applyPrepared(st, inputs)
	if err != nil {
		return Capacity{}, err
	}
	base, used, projected, err := projectCapacity(ctx, reader, st, next)
	if err != nil {
		return Capacity{}, err
	}
	return capacityFor(base, used, projected, lim), nil
}

// AddPrepared atomically stages a prepared batch. It takes its own service
// lock, reloads durable state, rechecks the remote HEAD, validates the exact
// projected capacity, and only then installs payloads. No caller-held lock is
// expected or accepted.
func (s *Service) AddPrepared(ctx context.Context, inputs []PreparedInput, opts PreparedOptions) ([]AssetView, error) {
	if opts.Reader == nil {
		return nil, fmt.Errorf("prepared add requires repository access")
	}
	limit, err := capacityLimit(opts.MaxBytes)
	if err != nil {
		return nil, err
	}
	unlock, err := s.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if err := mutable(st); err != nil {
		return nil, err
	}
	if err := validatePrepared(inputs, true); err != nil {
		return nil, err
	}
	next, err := applyPrepared(st, inputs)
	if err != nil {
		return nil, err
	}
	base, used, projected, err := projectCapacity(ctx, opts.Reader, st, next)
	if err != nil {
		return nil, err
	}
	capacity := capacityFor(base, used, projected, limit)
	if projected > capacity.LimitBytes {
		return nil, &CapacityError{Capacity: capacity}
	}
	return s.installPrepared(ctx, st, next, inputs)
}

// installPrepared installs already validated payloads and saves the staged
// state. The caller holds the service lock and has completed every capacity and
// occupancy check, so a failure here only rolls back payloads this call created.
func (s *Service) installPrepared(ctx context.Context, st, next index.State, inputs []PreparedInput) ([]AssetView, error) {
	success := false
	defer func() {
		if !success {
			_ = s.cleanup(context.WithoutCancel(ctx), st)
		}
	}()
	for i, input := range inputs {
		if _, _, err := staging.Install(ctx, s.Dir, input.Asset.Namespace, input.Payload); err != nil {
			discardTemps(inputs[i:])
			return nil, err
		}
	}
	if err := s.Store.Save(ctx, next); err != nil {
		return nil, err
	}
	success = true
	out := make([]AssetView, 0, len(inputs))
	for _, input := range inputs {
		out = append(out, AssetView{Asset: input.Asset, State: "added"})
	}
	return out, nil
}

// discardTemps removes ingest temporaries this call has not installed yet. The
// source bytes are read once; nothing outside the ingest directory is touched.
func discardTemps(inputs []PreparedInput) {
	for _, input := range inputs {
		if input.Payload.TempPath != "" {
			_ = os.Remove(input.Payload.TempPath)
		}
	}
}
