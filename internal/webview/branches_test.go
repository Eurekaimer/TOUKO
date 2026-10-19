package webview

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/github"
)

type branchTransport func(*http.Request) (*http.Response, error)

func (f branchTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func branchAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = branchTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.github.com" {
			t.Errorf("branch lookup escaped trusted GitHub origin: %s", r.URL.Redacted())
			return nil, fmt.Errorf("unexpected branch lookup origin")
		}
		out := httptest.NewRecorder()
		handler(out, r)
		return out.Result(), nil
	})
}

func TestBranchEndpointAggregatesProtectedAndUnicodeChoicesAcrossWorkspaces(t *testing.T) {
	f := newFixture(t)
	branchAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sentinel-private-token" || r.URL.Path != "/repos/owner/private/branches" {
			t.Error("private branch listing did not use server-side credentials and selected repository")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("Link", `<https://api.github.com/repos/owner/private/branches?page=2>; rel="next"`)
			fmt.Fprint(w, `[{"name":"main","protected":true}]`)
		case "2":
			fmt.Fprint(w, `[{"name":"发布/新版本","protected":false}]`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	for _, prefix := range []string{"/api", "/w/default/api"} {
		out := f.request("GET", prefix+"/branches?repository=owner%2Fprivate", nil, http.StatusOK)
		var result struct {
			Branches []github.BranchInfo `json:"branches"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		want := []github.BranchInfo{{Name: "main", Protected: true}, {Name: "发布/新版本"}}
		if !reflect.DeepEqual(result.Branches, want) {
			t.Fatalf("picker lost branches or protection information: %+v", result.Branches)
		}
		if strings.Contains(out.Body.String(), "sentinel-private-token") {
			t.Fatal("branch choices exposed server credentials")
		}
	}
}

func TestBranchEndpointAnonymousEmptyAndRemoteFailures(t *testing.T) {
	f := newFixture(t)
	f.server.token = ""
	f.server.authResolved = true
	f.server.authSource = "anonymous"
	branchAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("public anonymous branch lookup sent a credential")
		}
		switch r.URL.Path {
		case "/repos/owner/empty/branches":
			fmt.Fprint(w, `[]`)
		case "/repos/owner/private/branches":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case "/repos/owner/denied/branches":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible"}`)
		case "/repos/owner/limited/branches":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		case "/repos/owner/expired/branches":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"GitHub unavailable"}`)
		}
	})
	for _, tc := range []struct {
		repository string
		status     int
		code       string
		message    string
	}{
		{"owner/empty", http.StatusOK, "", ""},
		{"owner/private", http.StatusBadGateway, "api_error", "Not Found"},
		{"owner/denied", http.StatusForbidden, "forbidden", "Resource not accessible"},
		{"owner/limited", http.StatusBadGateway, "api_error", "API rate limit exceeded"},
		{"owner/expired", http.StatusUnauthorized, "unauthorized", "Bad credentials"},
		{"owner/failure", http.StatusBadGateway, "api_error", "GitHub unavailable"},
	} {
		t.Run(tc.repository, func(t *testing.T) {
			out := f.request("GET", "/api/branches?repository="+url.QueryEscape(tc.repository), nil, tc.status)
			if tc.status == http.StatusOK {
				if strings.TrimSpace(out.Body.String()) != `{"branches":[]}` {
					t.Fatalf("empty repository did not produce an empty picker: %s", out.Body.String())
				}
				return
			}
			var result struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error.Code != tc.code || !strings.Contains(result.Error.Message, tc.message) {
				t.Fatalf("remote failure became an empty or misleading picker: %s", out.Body.String())
			}
		})
	}
}

func TestBranchEndpointRejectsUntrustedSelectionAndRequests(t *testing.T) {
	f := newFixture(t)
	branchAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid branch request reached GitHub")
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, name := range []string{"", "owner", "owner/repo/extra", "https://github.com/owner/repo", "owner/..", "../repo", "owner/repo?token=secret", "owner/repo#fragment", "owner/repo\\escape", " owner/repo", "owner/仓库", "owner/repo\n"} {
		f.request("GET", "/api/branches?repository="+url.QueryEscape(name), nil, http.StatusBadRequest)
	}
	f.request("GET", "/api/branches", nil, http.StatusBadRequest)
	f.request("GET", "/api/branches?repository=owner/one&repository=owner/two", nil, http.StatusBadRequest)
	out := f.request("POST", "/api/branches?repository=owner/repo", map[string]any{}, http.StatusMethodNotAllowed)
	if out.Header().Get("Allow") != "GET" {
		t.Fatal("branch endpoint did not advertise GET-only access")
	}
	for _, tc := range []struct {
		header string
		value  string
	}{
		{"Host", "attacker.invalid"},
		{"Origin", "https://attacker.invalid"},
		{"Sec-Fetch-Site", "cross-site"},
	} {
		req := httptest.NewRequest("GET", testOrigin+"/w/default/api/branches?repository=owner/repo", nil)
		if tc.header == "Host" {
			req.Host = tc.value
		} else {
			req.Header.Set(tc.header, tc.value)
		}
		out := httptest.NewRecorder()
		f.server.ServeHTTP(out, req)
		if out.Code != http.StatusForbidden {
			t.Fatalf("untrusted %s reached branch endpoint: %d %s", tc.header, out.Code, out.Body.String())
		}
	}
}

func TestBranchEndpointRedactsRemoteErrors(t *testing.T) {
	f := newFixture(t)
	branchAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"failed sentinel-private-token"}`)
	})
	out := f.request("GET", "/api/branches?repository=owner/repo", nil, http.StatusBadGateway)
	if strings.Contains(out.Body.String(), "sentinel-private-token") || !strings.Contains(out.Body.String(), "[redacted]") {
		t.Fatal("remote branch error did not retain safe redacted detail")
	}
}
