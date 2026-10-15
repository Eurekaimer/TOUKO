package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	ingest "github.com/Eurekaimer/TOUKO/internal/image"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

type Input struct {
	Reader io.Reader
	Name   string
}
type AddOptions struct {
	Namespace         domain.Namespace
	Title             *string
	Tags, Collections []string
}
type EditOptions struct {
	Title             *string
	Tags, Collections *[]string
}
type AssetView struct {
	Asset domain.Asset `json:"asset"`
	State string       `json:"state"`
}

func normalize(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}
func mutable(st index.State) error {
	if st.Publication != nil {
		return fmt.Errorf("%w: unresolved publication; run sync", domain.ErrConflict)
	}
	if st.Manifest == nil || st.Head == "" {
		return fmt.Errorf("no committed snapshot; run sync before staging")
	}
	return nil
}
func bindBase(st *index.State) {
	if len(st.Changes) == 0 {
		st.BaseHead = ""
	} else if st.BaseHead == "" {
		st.BaseHead = st.Head
	}
}
func (s *Service) cleanup(ctx context.Context, st index.State) error {
	refs := make(map[string]bool)
	for _, c := range st.Changes {
		if c.Asset != nil {
			refs[staging.PayloadPath(s.Dir, c.Asset.Namespace, c.Asset.Object)] = true
		}
	}
	return staging.Cleanup(ctx, s.Dir, refs)
}
func (s *Service) Add(ctx context.Context, inputs []Input, opts AddOptions) ([]AssetView, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if err = mutable(st); err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("no input images")
	}
	ns, err := domain.ParseNamespace(string(opts.Namespace))
	if err != nil {
		return nil, err
	}
	original := st
	original.Changes = make(map[domain.AssetID]domain.Change, len(st.Changes))
	for id, c := range st.Changes {
		original.Changes[id] = c
	}
	success := false
	defer func() {
		if !success {
			_ = s.cleanup(context.WithoutCancel(ctx), original)
		}
	}()
	tempDir := filepath.Join(s.Dir, "stage", "tmp")
	if err = os.MkdirAll(tempDir, 0700); err != nil {
		return nil, err
	}
	out := make([]AssetView, 0, len(inputs))
	for _, input := range inputs {
		if input.Reader == nil {
			return nil, fmt.Errorf("nil input")
		}
		result, err := ingest.Ingest(ctx, input.Reader, tempDir)
		if err != nil {
			return nil, err
		}
		if _, _, err = staging.Install(ctx, s.Dir, ns, result); err != nil {
			return nil, err
		}
		id, err := domain.NewAssetID()
		if err != nil {
			return nil, err
		}
		name := input.Name
		if name == "" {
			name = "stdin." + result.Object.Ext
		} else {
			name = filepath.Base(name)
		}
		title := name
		if opts.Title != nil {
			title = *opts.Title
		}
		now := time.Now().UTC()
		a := domain.Asset{ID: id, Namespace: ns, Object: result.Object, OriginalName: name, Title: title, Tags: normalize(opts.Tags), Collections: normalize(opts.Collections), CreatedAt: now, UpdatedAt: now}
		st.Changes[id] = domain.Change{Kind: domain.Add, AssetID: id, Asset: &a}
		out = append(out, AssetView{Asset: a, State: "added"})
	}
	bindBase(&st)
	if err = s.Store.Save(ctx, st); err != nil {
		return nil, err
	}
	success = true
	return out, nil
}
func union(st index.State) map[domain.AssetID]domain.Asset {
	assets := make(map[domain.AssetID]domain.Asset, len(st.Assets)+len(st.Changes))
	for id, a := range st.Assets {
		assets[id] = a
	}
	for id, c := range st.Changes {
		if c.Kind == domain.Add && c.Asset != nil {
			assets[id] = *c.Asset
		}
	}
	return assets
}
func overlay(st index.State) []AssetView {
	assets := union(st)
	for id, c := range st.Changes {
		switch c.Kind {
		case domain.Delete:
			delete(assets, id)
		case domain.Update:
			assets[id] = *c.Asset
		}
	}
	out := make([]AssetView, 0, len(assets))
	for id, a := range assets {
		state := "committed"
		if c, ok := st.Changes[id]; ok {
			if c.Kind == domain.Add {
				state = "added"
			} else {
				state = "modified"
			}
		}
		out = append(out, AssetView{Asset: a, State: state})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Asset, out[j].Asset
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID.String() < b.ID.String()
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	return out
}
func (s *Service) List(ctx context.Context) ([]AssetView, error) {
	st, err := s.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	return overlay(st), nil
}
func (s *Service) Show(ctx context.Context, selector string) (AssetView, error) {
	st, err := s.Store.Load(ctx)
	if err != nil {
		return AssetView{}, err
	}
	visible := make(map[domain.AssetID]domain.Asset)
	views := overlay(st)
	for _, v := range views {
		visible[v.Asset.ID] = v.Asset
	}
	id, err := domain.ResolveAsset(visible, selector)
	if err != nil {
		return AssetView{}, err
	}
	for _, v := range views {
		if v.Asset.ID == id {
			return v, nil
		}
	}
	return AssetView{}, domain.ErrAssetNotFound
}
func (s *Service) Edit(ctx context.Context, selector string, opts EditOptions) error {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return err
	}
	if err = mutable(st); err != nil {
		return err
	}
	id, err := domain.ResolveAsset(union(st), selector)
	if err != nil {
		return err
	}
	c, changed := st.Changes[id]
	if changed && c.Kind == domain.Delete {
		return &domain.ConflictError{Base: st.BaseHead, Remote: st.Head, AssetID: &id, Reason: "delete_modify"}
	}
	a := st.Assets[id]
	if changed {
		a = *c.Asset
	}
	before := a
	if opts.Title != nil {
		a.Title = *opts.Title
	}
	if opts.Tags != nil {
		a.Tags = normalize(*opts.Tags)
	}
	if opts.Collections != nil {
		a.Collections = normalize(*opts.Collections)
	}
	if a.Title == before.Title && slices.Equal(a.Tags, before.Tags) && slices.Equal(a.Collections, before.Collections) {
		return nil
	}
	a.UpdatedAt = time.Now().UTC()
	kind := domain.Update
	if changed && c.Kind == domain.Add {
		kind = domain.Add
	}
	st.Changes[id] = domain.Change{Kind: kind, AssetID: id, Asset: &a}
	bindBase(&st)
	return s.Store.Save(ctx, st)
}
func (s *Service) Remove(ctx context.Context, selectors []string) error {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return err
	}
	if err = mutable(st); err != nil {
		return err
	}
	all := union(st)
	ids := make([]domain.AssetID, 0, len(selectors))
	for _, sel := range selectors {
		id, err := domain.ResolveAsset(all, sel)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	seen := make(map[domain.AssetID]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if c, ok := st.Changes[id]; ok && c.Kind == domain.Add {
			delete(st.Changes, id)
		} else {
			st.Changes[id] = domain.Change{Kind: domain.Delete, AssetID: id}
		}
	}
	bindBase(&st)
	if err = s.Store.Save(ctx, st); err != nil {
		return err
	}
	return s.cleanup(ctx, st)
}
