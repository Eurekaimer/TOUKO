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
