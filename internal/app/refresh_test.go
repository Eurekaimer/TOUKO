package app

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestSyncWithKeepsPullInsideWorkspaceLock(t *testing.T) {
	ctx, remote, service := realRepository(t)
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := service.SyncWith(ctx, func(context.Context, bool) (repository.Git, error) {
			close(entered)
			<-release
			return remote, nil
		})
		result <- err
	}()
	<-entered
	_, addErr := service.Add(ctx, []Input{{Reader: bytes.NewReader(pngBytes(t)), Name: "racing.png"}}, AddOptions{})
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(addErr, domain.ErrBusy) {
		t.Fatalf("staging interleaved with the remote pull: %v", addErr)
	}
	state, err := service.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Changes) != 0 {
		t.Fatal("a rejected concurrent mutation changed durable staging")
	}
}

func TestCloudflareRecoverySurvivesSecondRefresh(t *testing.T) {
	for _, refreshFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh_success", true: "refresh_failure"}[refreshFails], func(t *testing.T) {
			ctx, remote, service := realRepository(t)
			fake := newFakeMirror()
			enableTestCloudflare(t, service, fake, testCloudflareConfig())
			asset := stageImage(t, ctx, service)
			if _, err := service.Store.DB.ExecContext(ctx, `CREATE TRIGGER fail_cloudflare_head BEFORE UPDATE OF head ON repository_state BEGIN SELECT RAISE(ABORT, 'index failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Commit(ctx, remote, "published before cache fault"); err == nil {
				t.Fatal("cache failure hidden")
			}
			head, err := remote.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := repository.ReadSnapshot(ctx, remote, head)
			if err != nil || snapshot.Assets[asset.ID].Object != asset.Object {
				t.Fatal("publication missing before recovery", err)
			}
			key := repository.ObjectPath(asset)
			if _, exists := fake.object(key); exists {
				t.Fatal("mirror ran before durable cache finalization")
			}
			if _, err := service.Store.DB.ExecContext(ctx, `DROP TRIGGER fail_cloudflare_head`); err != nil {
				t.Fatal(err)
			}
			result, err := service.SyncWith(ctx, func(_ context.Context, recovering bool) (repository.Git, error) {
				if !recovering && refreshFails {
					return nil, errPublicationFault
				}
				return remote, nil
			})
			if refreshFails != errors.Is(err, errPublicationFault) || (!refreshFails && err != nil) {
				t.Fatal(err)
			}
			if result.Cloudflare == nil || result.Cloudflare.Status != "synced" {
				t.Fatalf("recovery lost mirror outcome: %+v", result)
			}
			if _, exists := fake.object(key); !exists {
				t.Fatal("recovered publication was not mirrored")
			}
			state, err := service.Store.Load(ctx)
			if err != nil || state.Publication != nil || len(state.Changes) != 0 || state.Head != head {
				t.Fatal(state, err)
			}
			after, err := remote.Head(ctx)
			if err != nil || after != head {
				t.Fatal("recovery republished", after, head, err)
			}
			fake.removeObject(key)
			normal, err := service.SyncWith(ctx, func(context.Context, bool) (repository.Git, error) { return remote, nil })
			if err != nil || normal.Cloudflare != nil {
				t.Fatal(normal, err)
			}
			if _, exists := fake.object(key); exists {
				t.Fatal("ordinary sync backfilled Cloudflare")
			}
		})
	}
}
