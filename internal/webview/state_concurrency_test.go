package webview

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

func TestCommittedPreviewRemainsAvailableDuringMutation(t *testing.T) {
	f := newFixture(t)
	f.setup()
	committedID, committedBytes := f.add()
	f.request("POST", "/api/commit", map[string]string{"message": "publish"}, http.StatusOK)
	stagedID, stagedBytes := f.add()

	preview := func(id string) <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			out := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, testOrigin+"/api/assets/"+id+"/image", nil)
			f.server.ServeHTTP(out, req)
			done <- out
		}()
		return done
	}

	f.server.workMu.Lock()
	committed := preview(committedID)
	select {
	case result := <-committed:
		if result.Code != http.StatusOK || !bytes.Equal(result.Body.Bytes(), committedBytes) {
			f.server.workMu.Unlock()
			t.Fatalf("committed preview: status %d", result.Code)
		}
	case <-time.After(time.Second):
		f.server.workMu.Unlock()
		<-committed
		t.Fatal("committed preview waited for a mutation")
	}

	staged := preview(stagedID)
	select {
	case result := <-staged:
		f.server.workMu.Unlock()
		t.Fatalf("staged preview bypassed payload protection: status %d", result.Code)
	case <-time.After(100 * time.Millisecond):
	}
	f.server.workMu.Unlock()
	select {
	case result := <-staged:
		if result.Code != http.StatusOK || !bytes.Equal(result.Body.Bytes(), stagedBytes) {
			t.Fatalf("staged preview: status %d", result.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("staged preview did not resume")
	}
}
