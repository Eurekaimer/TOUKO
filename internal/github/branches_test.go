package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	gh "github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestListBranchesIncludesAllPagesAndProtectedNames(t *testing.T) {
	want := make([]gh.BranchInfo, 100, 102)
	want[0] = gh.BranchInfo{Name: "main", Protected: true}
	for i := 1; i < 100; i++ {
		want[i] = gh.BranchInfo{Name: fmt.Sprintf("feature/%03d", i)}
	}
	want = append(want, gh.BranchInfo{Name: "发布/新版", Protected: true}, gh.BranchInfo{Name: "read-only"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/gallery/branches" || r.Header.Get("Authorization") != "Bearer "+sentinel || r.URL.Query().Get("per_page") != "100" || r.URL.Query().Has("protected") {
			t.Error("branch list changed repository, authentication, or branch visibility")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Query().Get("page") {
		case "1":
			// A full page still needs a following request when Link is absent.
			_ = json.NewEncoder(w).Encode(want[:100])
		case "2":
			// A short page with next must continue, but never to this URL.
			w.Header().Set("Link", `<https://untrusted.invalid/steal?page=3>; rel="next"`)
			_ = json.NewEncoder(w).Encode(want[100:101])
		case "3":
			_ = json.NewEncoder(w).Encode(want[101:])
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client, err := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}, sentinel, server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ListBranches(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("branches=%+v, want=%+v, err=%v", got, want, err)
	}
}

func TestListBranchesPublicEmptyRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("anonymous listing unexpectedly sent credentials")
		}
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	client, err := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "empty", Branch: "main"}, "", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ListBranches(context.Background())
	encoded, encodeErr := json.Marshal(got)
	if err != nil || encodeErr != nil || string(encoded) != "[]" {
		t.Fatalf("empty branches must encode as [], got=%s err=%v encodeErr=%v", encoded, err, encodeErr)
	}
}

func TestListBranchesLaterPageErrorsDoNotReturnPartialChoices(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "1" {
					w.Header().Set("Link", `<https://api.github.com/repos/owner/gallery/branches?page=2>; rel="next"`)
					fmt.Fprint(w, `[{"name":"main","protected":true}]`)
					return
				}
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "branch lookup failed " + sentinel})
			}))
			defer server.Close()
			client, err := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}, sentinel, server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.ListBranches(context.Background())
			var api *gh.APIError
			if got != nil || !errors.As(err, &api) || api.Status != status {
				t.Fatalf("partial branches=%+v error=%v", got, err)
			}
			if strings.Contains(err.Error(), sentinel) || strings.Contains(api.Message, sentinel) {
				t.Fatal("branch list error exposed a credential")
			}
			if errors.Is(err, domain.ErrUnauthorized) != (status == 401 || status == 403) {
				t.Fatalf("incorrect authentication classification: %v", err)
			}
		})
	}
}

func TestListBranchesRejectsMalformedOrTrailingJSON(t *testing.T) {
	for _, body := range []string{`{"name":"main"}`, `[{"name":"main"}] []`, `[{"name":`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			client, err := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}, "", server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.ListBranches(context.Background())
			if got != nil || err == nil || !strings.Contains(err.Error(), "invalid JSON") {
				t.Fatalf("invalid response became selectable branches: %+v, error=%v", got, err)
			}
		})
	}
}
