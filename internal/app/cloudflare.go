package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/delivery"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	ingest "github.com/Eurekaimer/TOUKO/internal/image"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/r2"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

// Cloudflare mirror ledger states. These strings are the durable contract of
// <state>/cloudflare/<target>.json.
const (
	cloudflarePending       = "pending"
	cloudflareSynced        = "synced"
	cloudflareFailed        = "failed"
	cloudflareDeletePending = "delete_pending"
	cloudflareDeleteFailed  = "delete_failed"
	cloudflareDeleted       = "deleted"
)

// Application-level codes. R2 adapter codes are enumerated separately so a
// fake or future adapter cannot inject arbitrary text into a receipt.
const (
	cloudflareCodeMirrorStateInvalid   = "mirror_state_invalid"
	cloudflareCodeMirrorStateWrite     = "mirror_state_write_failed"
	cloudflareCodeSourceChanged        = "source_changed"
	cloudflareCodeSourceUnavailable    = "source_unavailable"
	cloudflareCodeHashMismatch         = "hash_mismatch"
	cloudflareCodeDeferredPutFailure   = "deferred_put_failure"
	cloudflareCodeNotPublished         = "not_published"
	cloudflareCodeNotSynced            = "not_synced"
	cloudflareCodeLocalRepository      = "local_repository"
	cloudflareCodeConfigurationInvalid = "configuration_invalid"
	cloudflareCodeCredentialsMissing   = "credentials_missing"
	cloudflareCodeObjectConflict       = "object_conflict"
	cloudflareCodeNetwork              = "network"
	cloudflareCodeTimeout              = "timeout"
	cloudflareCodeCanceled             = "canceled"
	cloudflareCodeBucketUnavailable    = "bucket_unavailable"
	cloudflareCodeAccessDenied         = "access_denied"
	cloudflareCodeNotFound             = "not_found"
	cloudflareCodePreconditionFailed   = "precondition_failed"
)

type CloudflareOptions struct {
	DryRun bool
}

type CloudflareItem struct {
	ObjectPath string `json:"object_path"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	ErrorCode  string `json:"error_code,omitempty"`
}

type CloudflareResult struct {
	Status    string           `json:"status"`
	DryRun    bool             `json:"dry_run"`
	Uploaded  int              `json:"uploaded"`
	Skipped   int              `json:"skipped"`
	Deleted   int              `json:"deleted"`
	Pending   int              `json:"pending"`
	Failed    int              `json:"failed"`
	Items     []CloudflareItem `json:"items"`
	ErrorCode string           `json:"error_code,omitempty"`
}

// CloudflareSyncError reports an incomplete mirror reconciliation. Result is
// the structured, sanitized outcome. cause is retained only for errors.Is/As
// (notably context cancellation) and is never rendered by Error.
type CloudflareSyncError struct {
	Result CloudflareResult
	cause  error
}

func (e *CloudflareSyncError) Error() string {
	var b strings.Builder
	b.WriteString("cloudflare sync ")
	b.WriteString(e.Result.Status)
	if e.Result.ErrorCode != "" {
		b.WriteString(": ")
		b.WriteString(e.Result.ErrorCode)
	}
	if len(e.Result.Items) > 0 {
		fmt.Fprintf(&b, " (%d items)", len(e.Result.Items))
	}
	return b.String()
}

func (e *CloudflareSyncError) Unwrap() error { return e.cause }

type CloudflareAssetStatus struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type CloudflareStatus struct {
	Enabled        bool                             `json:"enabled"`
	Configured     bool                             `json:"configured"`
	ErrorCode      string                           `json:"error_code,omitempty"`
	PendingDeletes int                              `json:"pending_deletes"`
	Assets         map[string]CloudflareAssetStatus `json:"assets"`
}

type cloudflareRef struct {
	object domain.ContentObject
	entry  repository.GitEntry
}

type cloudflareReconcile struct {
	service    *Service
	repo       repository.Git
	cfg        r2.Config
	mirror     r2.Mirror
	repository domain.RepositoryID
	sourceID   string
	head       domain.RepositoryHead
	refs       map[string]cloudflareRef
	entries    map[string]index.MirrorEntry
	uploads    map[string]bool
	deletes    map[string]bool
	payloads   map[string]string
	dryRun     bool
	ledger     index.MirrorLedger

	result CloudflareResult
	cause  error
	stop   string
}

// CloudflareSync reconciles the full authoritative committed snapshot with the
// configured bucket. It never commits, resets, refreshes a clone or rewrites
// the committed cache.
func (s *Service) CloudflareSync(ctx context.Context, r repository.Git, opts CloudflareOptions) (CloudflareResult, error) {
	if err := ctx.Err(); err != nil {
		return cloudflareUnavailable("", err), err
	}
	unlock, err := s.Lock(ctx)
	if err != nil {
		return cloudflareUnavailable("", err), err
	}
	defer unlock()

	st, err := s.Store.Load(ctx)
	if err != nil {
		return cloudflareUnavailable("", err), err
	}
	if st.Publication != nil {
		return cloudflareUnavailable(cloudflareCodeNotPublished, nil),
			fmt.Errorf("%w: publication journal pending; run sync before cloudflare sync", domain.ErrConflict)
	}
	cfg, err := s.CloudflareConfig(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return cloudflareUnavailable("", err), err
		}
		code := cloudflareConfigCode(err)
		return cloudflareUnavailable(code, nil), cloudflareDeliveryError(code)
	}
	if !cfg.Enabled {
		return cloudflareUnavailable("", nil), cloudflareDeliveryError("")
	}
	location, err := s.Location(ctx)
	if err != nil {
		return cloudflareUnavailable(cloudflareCodeConfigurationInvalid, nil), cloudflareDeliveryError(cloudflareCodeConfigurationInvalid)
	}
	if location.Backend != "github" {
		return cloudflareUnavailable(cloudflareCodeLocalRepository, nil), cloudflareDeliveryError(cloudflareCodeLocalRepository)
	}
	head, err := r.Head(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return cloudflareUnavailable(cloudflareCodeSourceUnavailable, err), err
		}
		return cloudflareUnavailable(cloudflareCodeSourceUnavailable, nil), cloudflareDeliveryError(cloudflareCodeSourceUnavailable)
	}
	snapshot, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return cloudflareUnavailable(cloudflareCodeSourceUnavailable, err), err
		}
		return cloudflareUnavailable(cloudflareCodeSourceUnavailable, nil), err
	}
	if err = sameRepository(st, snapshot); err != nil {
		return cloudflareUnavailable(cloudflareCodeNotPublished, nil), err
	}
	if snapshot.Manifest.ID == (domain.RepositoryID{}) {
		return cloudflareUnavailable(cloudflareCodeNotPublished, nil), cloudflareDeliveryError(cloudflareCodeNotPublished)
	}
	refs, err := cloudflareSnapshotRefs(snapshot)
	if err != nil {
		return cloudflareUnavailable(cloudflareCodeSourceUnavailable, nil), err
	}
	ledger, entries, err := s.cloudflareLedger(ctx, cfg, snapshot.Manifest.ID, cloudflareSourceID(location))
	if err != nil {
		return cloudflareInternalResult(cloudflareCodeMirrorStateInvalid), &CloudflareSyncError{
			Result: cloudflareInternalResult(cloudflareCodeMirrorStateInvalid), cause: err,
		}
	}
	uploads := cloudflareUploadKeys(refs, nil, entries, false)
	deletes := cloudflareDeleteKeys(refs, entries)
	base := cloudflareNewReconcile(s, r, cfg, nil, snapshot.Manifest.ID, cloudflareSourceID(location), snapshot.Head, refs, entries, uploads, deletes, ledger, opts.DryRun)
	if s.NewCloudflare == nil {
		return cloudflareUnavailable(cloudflareCodeConfigurationInvalid, nil), cloudflareDeliveryError(cloudflareCodeConfigurationInvalid)
	}
	if len(uploads) == 0 && len(deletes) == 0 {
		return cloudflareEmptyResult(ctx, s.Dir, opts.DryRun)
	}
	mirror, err := s.cloudflareMirror(ctx, cfg)
	if err != nil {
		code := cloudflareConfigCode(err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return cloudflareUnavailable("", err), err
		}
		return cloudflareUnavailable(code, nil), cloudflareDeliveryError(code)
	}
	base.mirror = mirror
	result, cause := base.run(ctx)
	return cloudflareCompleted(result, cause, opts.DryRun)
}

// cloudflarePublication is the only automatic hook. It runs after a confirmed
// GitHub publication has been saved; it mirrors only new Add paths from this
// batch plus previously pending/deleted intents. Errors never become commit
// errors.
func (s *Service) cloudflarePublication(ctx context.Context, r repository.Git, st index.State, snap repository.Snapshot) *CloudflareResult {
	if err := ctx.Err(); err != nil {
		result := cloudflareInternalResult(cloudflareCodeCanceled)
		return &result
	}
	cfg, err := s.CloudflareConfig(ctx)
	if err != nil {
		result := cloudflareUnavailable(cloudflareConfigCode(err), err)
		return &result
	}
	if !cfg.Enabled {
		return nil
	}
	location, err := s.Location(ctx)
	if err != nil {
		result := cloudflareUnavailable(cloudflareCodeConfigurationInvalid, err)
		return &result
	}
	if location.Backend != "github" {
		return nil
	}
	if snap.Manifest.ID == (domain.RepositoryID{}) {
		result := cloudflareUnavailable(cloudflareCodeNotPublished, nil)
		return &result
	}
	refs, err := cloudflareSnapshotRefs(snap)
	if err != nil {
		result := cloudflareInternalResult(cloudflareCodeSourceUnavailable)
		return &result
	}
	ledger, entries, err := s.cloudflareLedger(ctx, cfg, snap.Manifest.ID, cloudflareSourceID(location))
	if err != nil {
		result := cloudflareInternalResult(cloudflareCodeMirrorStateInvalid)
		return &result
	}
	candidates := make(map[string]bool)
	payloads := make(map[string]string)
	for id, change := range st.Changes {
		if change.Kind != domain.Add || change.Asset == nil {
			continue
		}
		final, ok := snap.Assets[id]
		if !ok || final.Object != change.Asset.Object || final.Path != change.Asset.Path || final.Namespace != change.Asset.Namespace {
			continue
		}
		candidates[repository.ObjectPath(final)] = true
		payloads[repository.ObjectPath(final)] = staging.PayloadPath(s.Dir, final.Namespace, final.Object)
	}
	uploads := cloudflareUploadKeys(refs, candidates, entries, true)
	deletes := cloudflareDeleteKeys(refs, entries)
	if s.NewCloudflare == nil {
		result := cloudflareUnavailable(cloudflareCodeConfigurationInvalid, nil)
		return &result
	}
	if len(uploads) == 0 && len(deletes) == 0 {
		result, _ := cloudflareEmptyResult(ctx, s.Dir, false)
		return &result
	}
	mirror, err := s.cloudflareMirror(ctx, cfg)
	if err != nil {
		result := cloudflareUnavailable(cloudflareConfigCode(err), err)
		return &result
	}
	run := cloudflareNewReconcile(s, r, cfg, mirror, snap.Manifest.ID, cloudflareSourceID(location), snap.Head, refs, entries, uploads, deletes, ledger, false)
	run.payloads = payloads
	result, _ := run.run(ctx)
	return &result
}

func cloudflareNewReconcile(s *Service, repo repository.Git, cfg r2.Config, mirror r2.Mirror, repositoryID domain.RepositoryID, sourceID string, head domain.RepositoryHead, refs map[string]cloudflareRef, entries map[string]index.MirrorEntry, uploads, deletes map[string]bool, ledger index.MirrorLedger, dryRun bool) *cloudflareReconcile {
	return &cloudflareReconcile{
		service: s, repo: repo, cfg: cfg, mirror: mirror,
		repository: repositoryID, sourceID: sourceID, head: head,
		refs: refs, entries: entries, uploads: uploads, deletes: deletes,
		dryRun: dryRun, ledger: ledger,
		result: cloudflareResultShell(dryRun),
	}
}

func cloudflareResultShell(dryRun bool) CloudflareResult {
	status := "synced"
	if dryRun {
		status = "dry_run"
	}
	return CloudflareResult{Status: status, DryRun: dryRun, Items: []CloudflareItem{}}
}

func cloudflareEmptyResult(ctx context.Context, dir string, dryRun bool) (CloudflareResult, error) {
	if _, err := credential.ResolveR2(ctx, dir); err != nil {
		if ctx.Err() != nil {
			return cloudflareUnavailable(cloudflareAdapterCode(err), nil), err
		}
		return cloudflareUnavailable(cloudflareCodeCredentialsMissing, nil), cloudflareDeliveryError(cloudflareCodeCredentialsMissing)
	}
	return cloudflareResultShell(dryRun), nil
}

func cloudflareUnavailable(code string, cause error) CloudflareResult {
	r := CloudflareResult{Status: "unavailable", ErrorCode: code, Items: []CloudflareItem{}}
	_ = cause
	return r
}

func cloudflareInternalResult(code string) CloudflareResult {
	return CloudflareResult{Status: "failed", ErrorCode: code, Items: []CloudflareItem{}}
}

func cloudflareDeliveryError(code string) error {
	switch code {
	case "":
		return fmt.Errorf("%w: Cloudflare mirror is disabled or unavailable", domain.ErrDeliveryUnavailable)
	case cloudflareCodeNotSynced, cloudflareCodeNotPublished:
		return fmt.Errorf("%w: %s; run touko cloudflare sync", domain.ErrDeliveryUnavailable, code)
	default:
		return fmt.Errorf("%w: %s", domain.ErrDeliveryUnavailable, code)
	}
}

func cloudflareCompleted(result CloudflareResult, cause error, dryRun bool) (CloudflareResult, error) {
	if dryRun {
		if result.Failed == 0 {
			return result, nil
		}
		return result, &CloudflareSyncError{Result: result, cause: cause}
	}
	if result.Status == "synced" {
		return result, nil
	}
	return result, &CloudflareSyncError{Result: result, cause: cause}
}

func cloudflareConfigCode(err error) string {
	if err == nil {
		return ""
	}
	var settings *CloudflareConfigError
	if errors.As(err, &settings) {
		return settings.Code
	}
	switch r2.ErrorCode(err) {
	case cloudflareCodeConfigurationInvalid:
		return cloudflareCodeConfigurationInvalid
	case cloudflareCodeCredentialsMissing:
		return cloudflareCodeCredentialsMissing
	default:
		return cloudflareCodeConfigurationInvalid
	}
}

func cloudflareAdapterCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return cloudflareCodeCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return cloudflareCodeTimeout
	}
	switch r2.ErrorCode(err) {
	case cloudflareCodeNetwork, cloudflareCodeTimeout, cloudflareCodeCanceled,
		cloudflareCodeBucketUnavailable, cloudflareCodeAccessDenied,
		cloudflareCodeCredentialsMissing, cloudflareCodeConfigurationInvalid,
		cloudflareCodeNotFound, cloudflareCodePreconditionFailed, cloudflareCodeObjectConflict:
		return r2.ErrorCode(err)
	default:
		return cloudflareCodeNetwork
	}
}

func cloudflareTargetWide(code string) bool {
	switch code {
	case cloudflareCodeNetwork, cloudflareCodeTimeout, cloudflareCodeBucketUnavailable,
		cloudflareCodeAccessDenied, cloudflareCodeCredentialsMissing,
		cloudflareCodeConfigurationInvalid, cloudflareCodeCanceled, cloudflareCodeSourceUnavailable:
		return true
	}
	return false
}

func cloudflareSourceID(location repository.Location) string {
	raw, err := json.Marshal([]string{strings.ToLower(location.Owner), strings.ToLower(location.Name), location.Branch})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// NewCloudflareMirror is the production mirror factory. Credentials are always
// resolved by the workspace-scoped Service before this point, so the adapter
// never reads the process environment on its own.
func NewCloudflareMirror(_ context.Context, cfg r2.Config, pair credential.R2Pair) (r2.Mirror, error) {
	return r2.New(cfg, pair.AccessKeyID, pair.SecretAccessKey)
}

func (s *Service) cloudflareMirror(ctx context.Context, cfg r2.Config) (r2.Mirror, error) {
	if s.NewCloudflare == nil {
		return nil, fmt.Errorf("%w: Cloudflare mirror factory is not configured", domain.ErrDeliveryUnavailable)
	}
	pair, err := credential.ResolveR2(ctx, s.Dir)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &r2.Error{Op: "config", Code: cloudflareCodeCredentialsMissing, Cause: err}
	}
	mirror, err := s.NewCloudflare(ctx, cfg, pair)
	if err != nil {
		return nil, err
	}
	if mirror == nil {
		return nil, fmt.Errorf("%w: Cloudflare mirror factory returned no client", domain.ErrDeliveryUnavailable)
	}
	return mirror, nil
}

func cloudflareSnapshotRefs(snapshot repository.Snapshot) (map[string]cloudflareRef, error) {
	entries := make(map[string]repository.GitEntry, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		if entry.Type == "blob" {
			entries[entry.Path] = entry
		}
	}
	refs := make(map[string]cloudflareRef, len(snapshot.Assets))
	for _, asset := range snapshot.Assets {
		if err := asset.Object.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrRepositoryCorrupt, err)
		}
		path := repository.ObjectPath(asset)
		entry, ok := entries[path]
		if !ok || entry.Size != asset.Object.Size {
			return nil, fmt.Errorf("%w: missing object entry %s", domain.ErrRepositoryCorrupt, path)
		}
		if previous, ok := refs[path]; ok {
			if previous.object != asset.Object {
				return nil, fmt.Errorf("%w: contradictory descriptions for %s", domain.ErrRepositoryCorrupt, path)
			}
			continue
		}
		refs[path] = cloudflareRef{object: asset.Object, entry: entry}
	}
	return refs, nil
}

func cloudflareEntryMap(ledger index.MirrorLedger) map[string]index.MirrorEntry {
	out := make(map[string]index.MirrorEntry, len(ledger.Entries))
	for _, entry := range ledger.Entries {
		out[entry.Key] = entry
	}
	return out
}

func cloudflareSortedEntries(entries map[string]index.MirrorEntry) []index.MirrorEntry {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]index.MirrorEntry, 0, len(keys))
	for _, key := range keys {
		out = append(out, entries[key])
	}
	return out
}

func cloudflareUploadKeys(refs map[string]cloudflareRef, candidates map[string]bool, entries map[string]index.MirrorEntry, automatic bool) map[string]bool {
	out := make(map[string]bool)
	for key := range refs {
		if !automatic || candidates[key] {
			out[key] = true
		}
	}
	if automatic {
		for key, entry := range entries {
			switch entry.State {
			case cloudflarePending, cloudflareFailed, cloudflareDeletePending, cloudflareDeleteFailed, cloudflareDeleted:
			default:
				continue
			}
			if _, ok := refs[key]; ok {
				out[key] = true
			}
		}
	}
	return out
}

func cloudflareDeleteKeys(refs map[string]cloudflareRef, entries map[string]index.MirrorEntry) map[string]bool {
	out := make(map[string]bool)
	for key := range entries {
		if _, ok := refs[key]; !ok {
			out[key] = true
		}
	}
	return out
}

func cloudflareKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// cloudflareLedger loads and validates the target ledger. Missing is a zero
// ledger; a ledger for another repository/source is corruption and fails the
// target closed.
func (s *Service) cloudflareLedger(ctx context.Context, cfg r2.Config, repositoryID domain.RepositoryID, sourceID string) (index.MirrorLedger, map[string]index.MirrorEntry, error) {
	ledger, err := index.LoadMirror(ctx, s.Dir, cfg.TargetID())
	if err != nil {
		return index.MirrorLedger{}, nil, err
	}
	if ledger.Version == 0 {
		ledger = index.MirrorLedger{
			Version: 1, TargetID: cfg.TargetID(), Endpoint: cfg.Endpoint,
			Bucket: cfg.Bucket, RepositoryID: repositoryID, SourceID: sourceID,
			Entries: []index.MirrorEntry{},
		}
		return ledger, make(map[string]index.MirrorEntry), nil
	}
	if ledger.TargetID != cfg.TargetID() || ledger.Bucket != cfg.Bucket ||
		ledger.RepositoryID != repositoryID || ledger.SourceID != sourceID {
		return index.MirrorLedger{}, nil, fmt.Errorf("%w: mirror ledger identity differs from configuration", domain.ErrRepositoryCorrupt)
	}
	return ledger, cloudflareEntryMap(ledger), nil
}

func (r *cloudflareReconcile) addItem(path, action, status, code string) {
	r.result.Items = append(r.result.Items, CloudflareItem{ObjectPath: path, Action: action, Status: status, ErrorCode: code})
	switch status {
	case "synced":
		if action == "skip" {
			r.result.Skipped++
		} else {
			r.result.Uploaded++
		}
	case "deleted":
		r.result.Deleted++
	case "pending":
		r.result.Pending++
	case "failed":
		r.result.Failed++
	}
}

func (r *cloudflareReconcile) markEntry(key string, mutate func(*index.MirrorEntry)) {
	entry, ok := r.entries[key]
	if !ok {
		entry = index.MirrorEntry{Key: key}
	}
	mutate(&entry)
	r.entries[key] = entry
}

func (r *cloudflareReconcile) saveLedger(ctx context.Context) error {
	r.ledger.Entries = cloudflareSortedEntries(r.entries)
	return index.SaveMirror(ctx, r.service.Dir, r.ledger)
}

func (r *cloudflareReconcile) run(ctx context.Context) (CloudflareResult, error) {
	if !r.dryRun {
		if err := r.writeIntent(ctx); err != nil {
			r.result.Status = "failed"
			r.result.ErrorCode = cloudflareCodeMirrorStateWrite
			r.cause = err
			return r.result, err
		}
	}
	uploads := cloudflareKeys(r.uploads)
	deletes := cloudflareKeys(r.deletes)
	putsIncomplete := false
	type uploadResult struct {
		status, action, code, stop string
		cause                      error
		entry                      index.MirrorEntry
		hasEntry                   bool
	}
	for start := 0; start < len(uploads); start += 4 {
		if ctx.Err() != nil && r.stop == "" {
			r.stop = cloudflareCodeCanceled
			if r.cause == nil {
				r.cause = ctx.Err()
			}
		}
		if r.stop != "" {
			for _, key := range uploads[start:] {
				if !r.dryRun {
					r.markEntry(key, func(e *index.MirrorEntry) {
						e.State = cloudflarePending
						e.ErrorCode = r.stop
					})
				}
				r.addItem(key, "put", "pending", r.stop)
			}
			putsIncomplete = true
			break
		}
		end := min(start+4, len(uploads))
		results := make([]uploadResult, end-start)
		var wg sync.WaitGroup
		for i, key := range uploads[start:end] {
			child := *r
			child.entries = make(map[string]index.MirrorEntry, 1)
			if entry, ok := r.entries[key]; ok {
				child.entries[key] = entry
			}
			wg.Add(1)
			go func(i int, key string) {
				defer wg.Done()
				result := &results[i]
				if child.dryRun {
					result.status, result.action, result.code, result.stop, result.cause = child.reconcileUploadDry(ctx, key)
				} else {
					result.status, result.action, result.code, result.stop, result.cause = child.reconcileUpload(ctx, key)
					result.entry, result.hasEntry = child.entries[key]
				}
			}(i, key)
		}
		wg.Wait()
		for i, key := range uploads[start:end] {
			result := results[i]
			if result.hasEntry {
				r.entries[key] = result.entry
			}
			if result.cause != nil && r.cause == nil {
				r.cause = result.cause
			}
			if result.stop != "" && r.stop == "" {
				r.stop = result.stop
			}
			if result.status != "synced" && !(r.dryRun && (result.status == "exists" || result.status == "would_upload")) {
				putsIncomplete = true
			}
			r.addItem(key, result.action, result.status, result.code)
		}
	}
	if r.dryRun {
		if putsIncomplete {
			for _, key := range deletes {
				r.addItem(key, "delete", "pending", cloudflareCodeDeferredPutFailure)
			}
		} else {
			r.reconcileDeletesDry(ctx, deletes)
		}
	} else {
		if putsIncomplete {
			for _, key := range deletes {
				r.markEntry(key, func(e *index.MirrorEntry) {
					e.State = cloudflareDeletePending
					e.ErrorCode = cloudflareCodeDeferredPutFailure
				})
				r.addItem(key, "delete", "pending", cloudflareCodeDeferredPutFailure)
			}
		} else {
			r.reconcileDeletes(ctx, deletes)
		}
	}

	r.result.Uploaded = 0
	r.result.Skipped = 0
	r.result.Deleted = 0
	r.result.Pending = 0
	r.result.Failed = 0
	for _, item := range r.result.Items {
		switch item.Status {
		case "synced":
			if item.Action == "skip" {
				r.result.Skipped++
			} else {
				r.result.Uploaded++
			}
		case "exists":
			r.result.Skipped++
		case "deleted":
			r.result.Deleted++
		case "pending":
			r.result.Pending++
		case "failed":
			r.result.Failed++
		}
	}
	if r.dryRun {
		r.result.Status = "dry_run"
	} else if r.result.Failed > 0 {
		r.result.Status = "failed"
	} else if r.result.Pending > 0 {
		r.result.Status = "pending"
	} else {
		r.result.Status = "synced"
	}
	if r.result.ErrorCode == "" {
		if r.stop != "" {
			r.result.ErrorCode = r.stop
		} else {
			for _, item := range r.result.Items {
				if item.Status == "failed" && item.ErrorCode != "" {
					r.result.ErrorCode = item.ErrorCode
					break
				}
			}
		}
	}
	if r.dryRun {
		return r.result, r.cause
	}
	if err := r.saveResultLedger(ctx); err != nil {
		r.result.Status = "failed"
		r.result.ErrorCode = cloudflareCodeMirrorStateWrite
		r.cause = err
		return r.result, err
	}
	return r.result, r.cause
}

func (r *cloudflareReconcile) writeIntent(ctx context.Context) error {
	for key := range r.uploads {
		ref := r.refs[key]
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.Key = key
			e.Object = ref.object
			// Keep the last known ETag, but a pending entry is never a receipt.
			e.State = cloudflarePending
			e.ErrorCode = ""
		})
	}
	for key := range r.deletes {
		entry, ok := r.entries[key]
		if !ok {
			continue
		}
		entry.State = cloudflareDeletePending
		entry.ErrorCode = ""
		r.entries[key] = entry
	}
	return r.saveLedger(ctx)
}

func (r *cloudflareReconcile) saveResultLedger(ctx context.Context) error {
	saveContext := ctx
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// Only the local durable write is completed after cancellation.
		var cancel context.CancelFunc
		saveContext, cancel = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
	}
	return r.saveLedger(saveContext)
}

func (r *cloudflareReconcile) reconcileUploadDry(ctx context.Context, key string) (status, action, code, stop string, cause error) {
	ref := r.refs[key]
	info, exists, err := r.mirror.HeadObject(ctx, key)
	if err != nil {
		code = cloudflareAdapterCode(err)
		status, action = "failed", "put"
		if cloudflareTargetWide(code) {
			stop = code
		}
		return status, action, code, stop, err
	}
	if !exists {
		return "would_upload", "put", "", "", nil
	}
	if cloudflareObjectMatches(info, ref.object, r.repository.String(), r.sourceID) {
		return "exists", "skip", "", "", nil
	}
	if cloudflareOwnedMetadata(info, r.repository.String(), r.sourceID) {
		return "would_upload", "put", "", "", nil
	}
	return "failed", "put", cloudflareCodeObjectConflict, "", nil
}

func (r *cloudflareReconcile) reconcileUpload(ctx context.Context, key string) (status, action, code, stop string, cause error) {
	ref := r.refs[key]
	info, exists, err := r.mirror.HeadObject(ctx, key)
	if err != nil {
		code = cloudflareAdapterCode(err)
		status, action = "failed", "put"
		if cloudflareTargetWide(code) {
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.State = cloudflareFailed
				e.ErrorCode = code
			})
			return "failed", "put", code, code, err
		}
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.State = cloudflareFailed
			e.ErrorCode = code
		})
		return status, action, code, "", err
	}
	ifMatch := ""
	if exists {
		if cloudflareObjectMatches(info, ref.object, r.repository.String(), r.sourceID) {
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.Object = ref.object
				e.ETag = info.ETag
				e.State = cloudflareSynced
				e.ErrorCode = ""
			})
			return "synced", "skip", "", "", nil
		}
		if !cloudflareOwnedMetadata(info, r.repository.String(), r.sourceID) {
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.State = cloudflareFailed
				e.ErrorCode = cloudflareCodeObjectConflict
			})
			return "failed", "put", cloudflareCodeObjectConflict, "", nil
		}
		ifMatch = info.ETag
	}
	etag, localCode, err := r.putBlob(ctx, key, ref, ifMatch)
	if err != nil {
		code = localCode
		if code == "" {
			code = cloudflareAdapterCode(err)
		}
		if code == cloudflareCodePreconditionFailed {
			return r.recoverPrecondition(ctx, key, ref)
		}
		if cloudflareTargetWide(code) {
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.State = cloudflareFailed
				e.ErrorCode = code
			})
			return "failed", "put", code, code, err
		}
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.State = cloudflareFailed
			e.ErrorCode = code
		})
		return "failed", "put", code, "", err
	}
	confirmed, exists, err := r.mirror.HeadObject(ctx, key)
	if err != nil {
		code = cloudflareAdapterCode(err)
		if cloudflareTargetWide(code) {
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.State = cloudflareFailed
				e.ErrorCode = code
			})
			return "failed", "put", code, code, err
		}
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.State = cloudflareFailed
			e.ErrorCode = code
		})
		return "failed", "put", code, "", err
	}
	if !exists || !cloudflareObjectMatches(confirmed, ref.object, r.repository.String(), r.sourceID) {
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.State = cloudflareFailed
			e.ErrorCode = cloudflareCodeObjectConflict
		})
		return "failed", "put", cloudflareCodeObjectConflict, "", nil
	}
	if etag == "" {
		etag = confirmed.ETag
	}
	if etag == "" {
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.State = cloudflareFailed
			e.ErrorCode = cloudflareCodeObjectConflict
		})
		return "failed", "put", cloudflareCodeObjectConflict, "", nil
	}
	r.markEntry(key, func(e *index.MirrorEntry) {
		e.Object = ref.object
		e.ETag = etag
		e.State = cloudflareSynced
		e.ErrorCode = ""
	})
	return "synced", "put", "", "", nil
}

func (r *cloudflareReconcile) recoverPrecondition(ctx context.Context, key string, ref cloudflareRef) (status, action, code, stop string, cause error) {
	info, exists, err := r.mirror.HeadObject(ctx, key)
	if err != nil {
		code = cloudflareAdapterCode(err)
		if cloudflareTargetWide(code) {
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.State = cloudflareFailed
				e.ErrorCode = code
			})
			return "failed", "put", code, code, err
		}
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.State = cloudflareFailed
			e.ErrorCode = code
		})
		return "failed", "put", code, "", err
	}
	if !exists || !cloudflareObjectMatches(info, ref.object, r.repository.String(), r.sourceID) || !cloudflareValidETag(info.ETag) {
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.State = cloudflareFailed
			e.ErrorCode = cloudflareCodeObjectConflict
		})
		return "failed", "put", cloudflareCodeObjectConflict, "", nil
	}
	r.markEntry(key, func(e *index.MirrorEntry) {
		e.Object = ref.object
		e.ETag = info.ETag
		e.State = cloudflareSynced
		e.ErrorCode = ""
	})
	return "synced", "put", "", "", nil
}

func cloudflareObjectMatches(info r2.ObjectInfo, object domain.ContentObject, repositoryID, sourceID string) bool {
	return info.RepositoryID == repositoryID && info.SourceID == sourceID &&
		info.Size == object.Size && info.SHA256 == object.Hash.String() && cloudflareValidETag(info.ETag)
}

const cloudflareMaxObjectSize int64 = 100_000_000

func cloudflareValidSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func cloudflareValidETag(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

func cloudflareOwnedMetadata(info r2.ObjectInfo, repositoryID, sourceID string) bool {
	return info.RepositoryID == repositoryID && info.SourceID == sourceID &&
		cloudflareValidSHA256(info.SHA256) && info.Size > 0 && info.Size <= cloudflareMaxObjectSize &&
		cloudflareValidETag(info.ETag)
}

func (r *cloudflareReconcile) putBlob(ctx context.Context, key string, ref cloudflareRef, ifMatch string) (string, string, error) {
	if ref.entry.Type != "blob" || ref.entry.SHA == "" {
		return "", cloudflareCodeSourceUnavailable, fmt.Errorf("%w: object entry %s is unavailable", domain.ErrRepositoryCorrupt, key)
	}
	var body io.ReadCloser
	var err error
	if payload := r.payloads[key]; payload != "" {
		file, openErr := os.Open(payload)
		if openErr == nil {
			body = file
		} else if !errors.Is(openErr, os.ErrNotExist) {
			return "", cloudflareCodeSourceUnavailable, openErr
		}
	}
	if body == nil {
		body, err = r.repo.OpenBlob(ctx, repository.BlobID(ref.entry.SHA))
		if err != nil {
			return "", cloudflareCodeSourceUnavailable, err
		}
	}
	tempDir, err := os.MkdirTemp(r.service.Dir, "cloudflare-upload-")
	if err != nil {
		_ = body.Close()
		return "", cloudflareCodeMirrorStateWrite, err
	}
	defer os.RemoveAll(tempDir)
	ingested, ingestErr := ingest.Ingest(ctx, body, tempDir)
	closeErr := body.Close()
	if ingestErr != nil {
		code := cloudflareCodeHashMismatch
		if errors.Is(ingestErr, context.Canceled) || errors.Is(ingestErr, context.DeadlineExceeded) {
			code = cloudflareCodeCanceled
		}
		return "", code, ingestErr
	}
	if closeErr != nil {
		return "", cloudflareCodeSourceUnavailable, closeErr
	}
	defer os.Remove(ingested.TempPath)
	if ingested.Object != ref.object {
		return "", cloudflareCodeHashMismatch, fmt.Errorf("%w: object %s does not match the committed description", domain.ErrHashMismatch, key)
	}
	file, err := os.Open(ingested.TempPath)
	if err != nil {
		return "", cloudflareCodeSourceUnavailable, err
	}
	defer file.Close()
	etag, err := r.mirror.PutObject(ctx, key, file, r2.PutOptions{
		Object: ref.object, RepositoryID: r.repository, SourceID: r.sourceID, IfMatch: ifMatch,
	})
	if err != nil {
		return "", "", err
	}
	return etag, "", nil
}

func (r *cloudflareReconcile) reconcileDeletesDry(ctx context.Context, keys []string) {
	for i, key := range keys {
		entry, ok := r.entries[key]
		if !ok {
			continue
		}
		info, exists, err := r.mirror.HeadObject(ctx, key)
		if err != nil {
			code := cloudflareAdapterCode(err)
			r.addItem(key, "delete", "failed", code)
			if cloudflareTargetWide(code) {
				r.stop = code
				for _, remaining := range keys[i+1:] {
					r.addItem(remaining, "delete", "failed", code)
				}
			}
			if r.stop != "" {
				return
			}
			continue
		}
		if !exists || cloudflareOwnedForDelete(info, entry, r.repository.String(), r.sourceID) {
			r.addItem(key, "delete", "would_delete", "")
			continue
		}
		r.addItem(key, "delete", "failed", cloudflareCodeObjectConflict)
	}
}

func (r *cloudflareReconcile) reconcileDeletes(ctx context.Context, keys []string) {
	for i, key := range keys {
		entry, ok := r.entries[key]
		if !ok {
			continue
		}
		info, exists, err := r.mirror.HeadObject(ctx, key)
		if err != nil {
			code := cloudflareAdapterCode(err)
			if cloudflareTargetWide(code) {
				if r.cause == nil {
					r.cause = err
				}
				r.failDeleteAttempt(key, code)
				r.failRemainingDeletes(keys[i+1:], "pending", code)
				return
			}
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.State = cloudflareDeleteFailed
				e.ErrorCode = code
			})
			r.addItem(key, "delete", "failed", code)
			continue
		}
		if !exists {
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.State = cloudflareDeleted
				e.ErrorCode = ""
			})
			r.addItem(key, "delete", "deleted", "")
			continue
		}
		if !cloudflareOwnedForDelete(info, entry, r.repository.String(), r.sourceID) {
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.State = cloudflareDeleteFailed
				e.ErrorCode = cloudflareCodeObjectConflict
			})
			r.addItem(key, "delete", "failed", cloudflareCodeObjectConflict)
			continue
		}
		// The authoritative source may move between any two deletes. Re-check
		// immediately before each destructive call, never from a shallow clone.
		if r.repo == nil {
			r.failRemainingDeletes(keys[i:], "pending", cloudflareCodeSourceUnavailable)
			return
		}
		current, err := r.repo.Head(ctx)
		if err != nil {
			if r.cause == nil {
				r.cause = err
			}
			r.failRemainingDeletes(keys[i:], "pending", cloudflareCodeSourceUnavailable)
			return
		}
		if current != r.head {
			r.failRemainingDeletes(keys[i:], "pending", cloudflareCodeSourceChanged)
			return
		}
		if err := r.mirror.DeleteObject(ctx, key); err != nil {
			code := cloudflareAdapterCode(err)
			if cloudflareTargetWide(code) {
				if r.cause == nil {
					r.cause = err
				}
				r.failDeleteAttempt(key, code)
				r.failRemainingDeletes(keys[i+1:], "pending", code)
				return
			}
			r.markEntry(key, func(e *index.MirrorEntry) {
				e.State = cloudflareDeleteFailed
				e.ErrorCode = code
			})
			r.addItem(key, "delete", "failed", code)
			continue
		}
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.State = cloudflareDeleted
			e.ErrorCode = ""
		})
		r.addItem(key, "delete", "deleted", "")
	}
}

func cloudflareOwnedForDelete(info r2.ObjectInfo, entry index.MirrorEntry, repositoryID, sourceID string) bool {
	return info.RepositoryID == repositoryID && info.SourceID == sourceID &&
		cloudflareValidSHA256(info.SHA256) && info.Size > 0 && info.Size <= cloudflareMaxObjectSize &&
		info.SHA256 == entry.Object.Hash.String() && info.Size == entry.Object.Size
}

func (r *cloudflareReconcile) failDeleteAttempt(key, code string) {
	r.markEntry(key, func(e *index.MirrorEntry) {
		e.State = cloudflareDeleteFailed
		e.ErrorCode = code
	})
	r.addItem(key, "delete", "failed", code)
}

func (r *cloudflareReconcile) failRemainingDeletes(keys []string, status, code string) {
	if r.result.ErrorCode == "" {
		r.result.ErrorCode = code
	}
	for _, key := range keys {
		r.markEntry(key, func(e *index.MirrorEntry) {
			e.State = cloudflareDeletePending
			e.ErrorCode = code
		})
		r.addItem(key, "delete", status, code)
	}
}

// CloudflareURL requires a committed asset with a matching synced receipt and a
// live HEAD confirmation. It never synthesizes a URL from a staged asset.
func (s *Service) CloudflareURL(ctx context.Context, selector string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	unlock, err := s.Lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	cfg, err := s.CloudflareConfig(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", cloudflareDeliveryError(cloudflareConfigCode(err))
	}
	if !cfg.Enabled {
		return "", cloudflareDeliveryError("")
	}
	location, err := s.Location(ctx)
	if err != nil {
		return "", cloudflareDeliveryError(cloudflareCodeConfigurationInvalid)
	}
	if location.Backend != "github" {
		return "", cloudflareDeliveryError(cloudflareCodeLocalRepository)
	}
	st, err := s.Store.Load(ctx)
	if err != nil {
		return "", err
	}
	if st.Manifest == nil {
		return "", cloudflareDeliveryError(cloudflareCodeNotPublished)
	}
	if _, err := credential.ResolveR2(ctx, s.Dir); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", cloudflareDeliveryError(cloudflareCodeCredentialsMissing)
	}
	id, err := domain.ResolveAsset(st.Assets, selector)
	if err != nil {
		if _, stagedErr := domain.ResolveAsset(union(st), selector); stagedErr == nil {
			return "", cloudflareDeliveryError(cloudflareCodeNotPublished)
		}
		return "", err
	}
	asset := st.Assets[id]
	objectPath := repository.ObjectPath(asset)
	sourceID := cloudflareSourceID(location)
	ledger, entries, err := s.cloudflareLedger(ctx, cfg, st.Manifest.ID, sourceID)
	if err != nil {
		return "", cloudflareDeliveryError(cloudflareCodeMirrorStateInvalid)
	}
	entry, ok := entries[objectPath]
	if !ok || entry.State != cloudflareSynced || entry.Object != asset.Object || entry.ETag == "" {
		return "", cloudflareDeliveryError(cloudflareCodeNotSynced)
	}
	mirror, err := s.cloudflareMirror(ctx, cfg)
	if err != nil {
		return "", cloudflareDeliveryError(cloudflareConfigCode(err))
	}
	info, exists, err := mirror.HeadObject(ctx, objectPath)
	if err != nil {
		code := cloudflareAdapterCode(err)
		if saveErr := s.cloudflareURLReceipt(ctx, ledger, entries, objectPath, cloudflareFailed, code); saveErr != nil {
			return "", cloudflareDeliveryError(cloudflareCodeMirrorStateWrite)
		}
		return "", cloudflareDeliveryError(code)
	}
	if !exists {
		if saveErr := s.cloudflareURLReceipt(ctx, ledger, entries, objectPath, cloudflarePending, cloudflareCodeNotFound); saveErr != nil {
			return "", cloudflareDeliveryError(cloudflareCodeMirrorStateWrite)
		}
		return "", cloudflareDeliveryError(cloudflareCodeNotSynced)
	}
	if !cloudflareObjectMatches(info, asset.Object, st.Manifest.ID.String(), sourceID) {
		if saveErr := s.cloudflareURLReceipt(ctx, ledger, entries, objectPath, cloudflareFailed, cloudflareCodeObjectConflict); saveErr != nil {
			return "", cloudflareDeliveryError(cloudflareCodeMirrorStateWrite)
		}
		return "", cloudflareDeliveryError(cloudflareCodeObjectConflict)
	}
	url, err := delivery.BuildCloudflareURL(cfg.PublicBaseURL, objectPath)
	if err != nil {
		return "", cloudflareDeliveryError(cloudflareCodeConfigurationInvalid)
	}
	return url, nil
}

func (s *Service) cloudflareURLReceipt(ctx context.Context, ledger index.MirrorLedger, entries map[string]index.MirrorEntry, key, state, code string) error {
	entry, ok := entries[key]
	if !ok {
		return nil
	}
	entry.State = state
	entry.ErrorCode = code
	entries[key] = entry
	ledger.Entries = cloudflareSortedEntries(entries)
	if err := index.SaveMirror(ctx, s.Dir, ledger); err != nil {
		return fmt.Errorf("invalidate mirror receipt: %w", err)
	}
	return nil
}

// CloudflareStatus derives UI state from the caller's single ReadView and the
// local ledger. It never probes the network or constructs an R2 client.
func (s *Service) CloudflareStatus(ctx context.Context, st index.State, views []AssetView) *CloudflareStatus {
	cfg, err := s.CloudflareConfig(ctx)
	if err != nil {
		return &CloudflareStatus{
			Enabled: true, Configured: false, ErrorCode: cloudflareConfigCode(err),
			Assets: map[string]CloudflareAssetStatus{},
		}
	}
	if !cfg.Enabled {
		return nil
	}
	status := &CloudflareStatus{Enabled: true, Assets: map[string]CloudflareAssetStatus{}}
	if st.Manifest == nil {
		status.ErrorCode = cloudflareCodeNotPublished
		return status
	}
	location, err := s.Location(ctx)
	if err != nil || location.Backend != "github" {
		status.ErrorCode = cloudflareCodeLocalRepository
		return status
	}
	if _, err := credential.ResolveR2(ctx, s.Dir); err != nil {
		status.ErrorCode = cloudflareCodeCredentialsMissing
		return status
	}
	sourceID := cloudflareSourceID(location)
	ledger, entries, err := s.cloudflareLedger(ctx, cfg, st.Manifest.ID, sourceID)
	if err != nil {
		status.ErrorCode = cloudflareCodeMirrorStateInvalid
		return status
	}
	status.Configured = true
	for _, entry := range ledger.Entries {
		if entry.State == cloudflareDeletePending || entry.State == cloudflareDeleteFailed {
			status.PendingDeletes++
		}
	}
	for _, view := range views {
		committed, committedOK := st.Assets[view.Asset.ID]
		objectPath := repository.ObjectPath(view.Asset)
		entry, hasEntry := entries[objectPath]
		state := cloudflarePending
		reason := ""
		if committedOK && committed.Object == view.Asset.Object && hasEntry &&
			entry.State == cloudflareSynced && entry.Object == committed.Object {
			state = cloudflareSynced
		} else if hasEntry && entry.State == cloudflareFailed {
			state = cloudflareFailed
			reason = entry.ErrorCode
		}
		status.Assets[view.Asset.ID.String()] = CloudflareAssetStatus{State: state, Reason: reason}
	}
	return status
}
