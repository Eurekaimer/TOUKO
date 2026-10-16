package github_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	gh "github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestListRepositoriesIncludesLaterPagesAndPermissions(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" || r.Header.Get("Authorization") != "Bearer "+sentinel {
			t.Error("repository listing did not use authenticated account endpoint")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		pages = append(pages, r.URL.Query().Get("page"))
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("Link", `<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=3>; rel="last"`)
			fmt.Fprint(w, `[{"full_name":"owner/first","default_branch":"main","private":false,"permissions":{"push":true}}]`)
		case "2":
			w.Header().Set("Link", `<https://api.github.com/user/repos?page=3>; rel="next", <https://api.github.com/user/repos?page=1>; rel="prev"`)
			fmt.Fprint(w, `[{"full_name":"org/private","default_branch":"release","private":true,"permissions":{"push":false}}]`)
		case "3":
			fmt.Fprint(w, `[{"full_name":"shared/last","default_branch":"trunk","private":false,"permissions":{"push":true}}]`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client, err := gh.NewWithClient(repository.Location{Backend: "github", Owner: "account", Name: "list", Branch: "main"}, sentinel, server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ListRepositories(context.Background())
	want := []gh.RepositoryInfo{
		{FullName: "owner/first", DefaultBranch: "main", CanPush: true},
		{FullName: "org/private", DefaultBranch: "release", Private: true},
		{FullName: "shared/last", DefaultBranch: "trunk", CanPush: true},
	}
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(pages, []string{"1", "2", "3"}) {
		t.Fatalf("repositories=%+v pages=%v err=%v", got, pages, err)
	}
}
