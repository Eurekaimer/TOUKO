package webview

import (
	"net/http"
	"testing"
	"time"
)

func TestWorkspaceStateReadableWhileWorkMutexHeld(t *testing.T) {
	f, _, members, _ := createLibraryFixture(t)
	server, err := f.server.spaces.open(t.Context(), members[0], false)
	if err != nil {
		t.Fatal(err)
	}
	server.workMu.Lock()
	locked := true
	defer func() {
		if locked {
			server.workMu.Unlock()
		}
	}()
	stateDone := make(chan int, 1)
	go func() { stateDone <- libraryCall(t, f, "GET", "/w/"+members[0]+"/api/state", nil, "").Code }()
	select {
	case code := <-stateDone:
		if code != http.StatusOK {
			t.Fatalf("state status %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("state waited for work mutex")
	}
	writeDone := make(chan int, 1)
	go func() {
		writeDone <- libraryCall(t, f, "POST", "/w/"+members[0]+"/api/reset", map[string]bool{"confirm": true}, server.csrf).Code
	}()
	select {
	case code := <-writeDone:
		t.Fatalf("write passed held mutex: %d", code)
	case <-time.After(100 * time.Millisecond):
	}
	server.workMu.Unlock()
	locked = false
	select {
	case code := <-writeDone:
		if code != http.StatusOK {
			t.Fatalf("write status %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write did not resume")
	}
}
