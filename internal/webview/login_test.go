package webview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newBrowserLoginFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	f := newFixture(t)
	f.server.token = ""
	f.server.authMu.Lock()
	f.server.authToken = ""
	f.server.authSource = ""
	f.server.authLogin = ""
	f.server.authResolved = false
	f.server.authMu.Unlock()
	f.server.login.lookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	f.server.login.open = func(context.Context, string) error { return nil }
	t.Cleanup(f.server.shutdownLogin)
	return f
}

func decodeBrowserLogin(t *testing.T, out *httptest.ResponseRecorder) loginState {
	t.Helper()
	var state loginState
	if err := json.Unmarshal(out.Body.Bytes(), &state); err != nil {
		t.Fatalf("invalid login response %q: %v", out.Body.String(), err)
	}
	return state
}

func browserLoginRequest(t *testing.T, f *fixture, method, path, body, origin, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, testOrigin+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if csrf != "" {
		request.Header.Set("X-Touko-CSRF", csrf)
	}
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, request)
	return out
}

func waitBrowserLogin(t *testing.T, f *fixture, want string) loginState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := decodeBrowserLogin(t, f.request("GET", "/api/auth/login", nil, http.StatusOK))
		if state.Status == want {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("login status = %q, want %q: %s", state.Status, want, state.Message)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBrowserLoginParsesOnlyDeviceProtocol(t *testing.T) {
	var output loginOutput
	if code, notify := output.parse("! First copy your one-time code: abcd-efgh"); notify || code != "" {
		t.Fatalf("notified before the device URL: %q %t", code, notify)
	}
	if output.code != "ABCD-EFGH" || output.url {
		t.Fatalf("code parse = %+v", output)
	}
	code, notify := output.parse("Open this URL to continue in your web browser: https://github.com/login/device")
	if !notify || code != "ABCD-EFGH" {
		t.Fatalf("valid device pair did not notify once: %q %t", code, notify)
	}
	if _, again := output.parse("https://github.com/login/device"); again {
		t.Fatal("the same pair notified twice")
	}

	var rejected loginOutput
	rejected.parse("! First copy your one-time code: ABCDEFGH")
	rejected.parse("Open https://evil.example/login/device abcd-efgh")
	rejected.parse("Open https://github.com/login/device")
	if rejected.code != "" || rejected.notified {
		t.Fatalf("malformed or foreign device output was accepted: %+v", rejected)
	}
	rejected.parse("warning: storing token in plaintext")
	if !rejected.plaintext {
		t.Fatal("keyring fallback warning was not retained")
	}
}

func TestBrowserLoginStartRequiresRootCSRFAndSameOrigin(t *testing.T) {
	f := newBrowserLoginFixture(t)
	var starts atomic.Int32
	f.server.login.run = func(ctx context.Context, _ func(string)) loginRunResult {
		starts.Add(1)
		<-ctx.Done()
		return loginRunResult{ExitCode: -1, Err: ctx.Err()}
	}

	passive := decodeBrowserLogin(t, f.request("GET", "/api/auth/login", nil, http.StatusOK))
	if passive.Status != loginIdle || !passive.Available || passive.BlockedSource != "" || passive.CSRF != f.csrf {
		t.Fatalf("passive login status = %+v", passive)
	}
	member := decodeBrowserLogin(t, f.request("GET", "/w/default/api/auth/login", nil, http.StatusOK))
	if member.CSRF != f.csrf || member.Status != loginIdle {
		t.Fatalf("member login status did not expose the root state: %+v", member)
	}
	if starts.Load() != 0 {
		t.Fatal("GET /api/auth/login started a login process")
	}

	for _, tc := range []struct {
		name   string
		path   string
		origin string
		csrf   string
	}{
		{"missing csrf", "/api/auth/login", testOrigin, ""},
		{"missing origin", "/api/auth/login", "", f.csrf},
		{"cross origin", "/api/auth/login", "https://evil.example", f.csrf},
		{"member missing csrf", "/w/default/api/auth/login", testOrigin, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := browserLoginRequest(t, f, http.MethodPost, tc.path, `{}`, tc.origin, tc.csrf)
			if out.Code != http.StatusForbidden {
				t.Fatalf("login mutation status = %d, want 403: %s", out.Code, out.Body.String())
			}
		})
	}
	if starts.Load() != 0 {
		t.Fatal("rejected requests started a login process")
	}
}

func TestBrowserLoginStartIsIdempotentAndCancelJoins(t *testing.T) {
	f := newBrowserLoginFixture(t)
	var starts, opens atomic.Int32
	cancelled := make(chan struct{})
	var once sync.Once
	f.server.login.open = func(_ context.Context, target string) error {
		if target != githubDeviceURL {
			t.Errorf("opened %q, want %q", target, githubDeviceURL)
		}
		opens.Add(1)
		return nil
	}
	f.server.login.run = func(ctx context.Context, onCode func(string)) loginRunResult {
		starts.Add(1)
		onCode("ABCD-EFGH")
		<-ctx.Done()
		once.Do(func() { close(cancelled) })
		return loginRunResult{ExitCode: -1, Err: ctx.Err()}
	}

	first := decodeBrowserLogin(t, browserLoginRequest(t, f, http.MethodPost, "/w/default/api/auth/login", `{}`, testOrigin, f.csrf))
	if first.Status != loginStarting && first.Status != loginWaiting {
		t.Fatalf("first start status = %q: %s", first.Status, first.Message)
	}
	if first.CSRF != f.csrf || !first.Available || first.BlockedSource != "" {
		t.Fatalf("start response omitted shared login fields: %+v", first)
	}
	second := decodeBrowserLogin(t, browserLoginRequest(t, f, http.MethodPost, "/api/auth/login", `{}`, testOrigin, f.csrf))
	if second.Status == loginFailed || second.Status == loginCancelled {
		t.Fatalf("second start replaced the active session: %+v", second)
	}
	waiting := waitBrowserLogin(t, f, loginWaiting)
	if waiting.UserCode != "ABCD-EFGH" || waiting.VerificationURL != githubDeviceURL {
		t.Fatalf("device code state = %+v", waiting)
	}
	deadline := time.Now().Add(3 * time.Second)
	for opens.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starts.Load() != 1 || opens.Load() != 1 {
		t.Fatalf("starts=%d opens=%d, want one of each", starts.Load(), opens.Load())
	}

	stopped := decodeBrowserLogin(t, f.request("POST", "/api/auth/login/cancel", map[string]any{}, http.StatusOK))
	if stopped.Status != loginCancelled || stopped.UserCode != "" || stopped.VerificationURL != "" {
		t.Fatalf("cancelled state still exposed a code: %+v", stopped)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("cancel returned before the runner stopped")
	}
}

func TestBrowserLoginFailureClearsCodeAndHidesRawOutput(t *testing.T) {
	f := newBrowserLoginFixture(t)
	f.server.login.run = func(_ context.Context, onCode func(string)) loginRunResult {
		onCode("WXYZ-2345")
		return loginRunResult{ExitCode: 3, Err: errors.New("sentinel private stderr")}
	}
	f.request("POST", "/api/auth/login", map[string]any{}, http.StatusOK)
	failed := waitBrowserLogin(t, f, loginFailed)
	if failed.UserCode != "" || failed.VerificationURL != "" {
		t.Fatalf("failed state retained a device code: %+v", failed)
	}
	if !strings.Contains(failed.Message, "exit status 3") {
		t.Fatalf("failure did not report its exit status: %s", failed.Message)
	}
	out := f.request("GET", "/api/auth/login", nil, http.StatusOK)
	if strings.Contains(out.Body.String(), "sentinel private stderr") {
		t.Fatal("raw runner stderr reached the page")
	}
}

func TestBrowserLoginBrowserFailureKeepsCodeForManualEntry(t *testing.T) {
	f := newBrowserLoginFixture(t)
	f.server.login.open = func(context.Context, string) error { return errors.New("launcher unavailable") }
	f.server.login.run = func(ctx context.Context, onCode func(string)) loginRunResult {
		onCode("ABCD-EFGH")
		<-ctx.Done()
		return loginRunResult{ExitCode: -1, Err: ctx.Err()}
	}
	if out := browserLoginRequest(t, f, http.MethodPost, "/api/auth/login", `{}`, testOrigin, f.csrf); out.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", out.Code, out.Body.String())
	}
	waiting := waitBrowserLogin(t, f, loginWaiting)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(waiting.Message, "Could not open your browser") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		waiting = decodeBrowserLogin(t, f.request("GET", "/api/auth/login", nil, http.StatusOK))
	}
	if waiting.UserCode != "ABCD-EFGH" || waiting.VerificationURL != githubDeviceURL {
		t.Fatalf("browser failure discarded the manual code: %+v", waiting)
	}
	if !strings.Contains(waiting.Message, "Could not open your browser") || !strings.Contains(waiting.Message, githubDeviceURL) {
		t.Fatalf("browser failure was not explained: %s", waiting.Message)
	}
	stopped := decodeBrowserLogin(t, f.request("POST", "/api/auth/login/cancel", map[string]any{}, http.StatusOK))
	if stopped.Status != loginCancelled {
		t.Fatalf("cancel status = %q", stopped.Status)
	}
}

func TestBrowserLoginOpenReopensOnlyWhileWaiting(t *testing.T) {
	f := newBrowserLoginFixture(t)
	var opens atomic.Int32
	f.server.login.open = func(_ context.Context, target string) error {
		if target != githubDeviceURL {
			t.Errorf("opened %q, want %q", target, githubDeviceURL)
		}
		opens.Add(1)
		return nil
	}
	f.server.login.run = func(ctx context.Context, onCode func(string)) loginRunResult {
		onCode("ABCD-EFGH")
		<-ctx.Done()
		return loginRunResult{ExitCode: -1, Err: ctx.Err()}
	}
	f.request("POST", "/api/auth/login", map[string]any{}, http.StatusOK)
	waiting := waitBrowserLogin(t, f, loginWaiting)
	deadline := time.Now().Add(3 * time.Second)
	for opens.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if opens.Load() != 1 {
		t.Fatalf("automatic browser opens = %d, want 1", opens.Load())
	}

	reopened := decodeBrowserLogin(t, f.request("POST", "/api/auth/login/open", map[string]any{}, http.StatusOK))
	opened := opens.Load()
	if reopened.Status != loginWaiting || reopened.UserCode != waiting.UserCode || opened != 2 {
		t.Fatalf("manual reopen lost the code or did not open once: %+v opens=%d", reopened, opened)
	}
	if strings.Contains(reopened.Message, "Could not open your browser") || !strings.Contains(reopened.Message, githubDeviceURL) {
		t.Fatalf("successful reopen kept a failure message: %s", reopened.Message)
	}

	stopped := decodeBrowserLogin(t, f.request("POST", "/api/auth/login/cancel", map[string]any{}, http.StatusOK))
	if stopped.Status != loginCancelled {
		t.Fatalf("cancel status = %q", stopped.Status)
	}
	rejected := browserLoginRequest(t, f, http.MethodPost, "/api/auth/login/open", `{}`, testOrigin, f.csrf)
	if rejected.Code != http.StatusBadRequest || opens.Load() != 2 {
		t.Fatalf("reopen after cancel = %d opens=%d: %s", rejected.Code, opens.Load(), rejected.Body.String())
	}
}

func TestBrowserLoginTimeoutStopsProcessAndClearsCode(t *testing.T) {
	f := newBrowserLoginFixture(t)
	f.server.login.lifetime = 25 * time.Millisecond
	stopped := make(chan struct{})
	f.server.login.run = func(ctx context.Context, onCode func(string)) loginRunResult {
		onCode("ABCD-EFGH")
		<-ctx.Done()
		close(stopped)
		return loginRunResult{ExitCode: -1, Err: ctx.Err()}
	}
	f.request("POST", "/api/auth/login", map[string]any{}, http.StatusOK)
	failed := waitBrowserLogin(t, f, loginFailed)
	if failed.UserCode != "" || failed.VerificationURL != "" {
		t.Fatalf("timed-out state retained a code: %+v", failed)
	}
	if !strings.Contains(failed.Message, "timed out") {
		t.Fatalf("timeout was not explained: %s", failed.Message)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout left the runner running")
	}
}

func TestBrowserLoginShutdownStopsBackgroundProcess(t *testing.T) {
	f := newBrowserLoginFixture(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	var startOnce, stopOnce sync.Once
	f.server.login.run = func(ctx context.Context, _ func(string)) loginRunResult {
		startOnce.Do(func() { close(started) })
		<-ctx.Done()
		stopOnce.Do(func() { close(stopped) })
		return loginRunResult{ExitCode: -1, Err: ctx.Err()}
	}
	f.request("POST", "/api/auth/login", map[string]any{}, http.StatusOK)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("login runner did not start")
	}
	f.server.shutdownLogin()
	select {
	case <-stopped:
	default:
		t.Fatal("shutdown returned before the runner stopped")
	}
	state := decodeBrowserLogin(t, f.request("GET", "/api/auth/login", nil, http.StatusOK))
	if state.Status != loginCancelled {
		t.Fatalf("shutdown status = %q, want cancelled", state.Status)
	}
}

func TestBrowserLoginSuccessRequiresStoredCredential(t *testing.T) {
	f := newBrowserLoginFixture(t)
	f.server.authResolve = func(context.Context) (string, string, error) {
		return "", "anonymous", nil
	}
	f.server.login.run = func(_ context.Context, onCode func(string)) loginRunResult {
		onCode("ABCD-EFGH")
		return loginRunResult{}
	}
	f.request("POST", "/api/auth/login", map[string]any{}, http.StatusOK)
	failed := waitBrowserLogin(t, f, loginFailed)
	if failed.UserCode != "" || failed.VerificationURL != "" {
		t.Fatalf("missing credential retained a device code: %+v", failed)
	}
	if strings.Contains(failed.Message, "Signed in") || !strings.Contains(failed.Message, "could not read the stored credential") {
		t.Fatalf("gh exit zero without a credential was reported as success: %s", failed.Message)
	}
	token, source, err := f.server.credentials(t.Context(), false)
	if err != nil || token != "" || source != "anonymous" {
		t.Fatalf("missing credential cache = %q %q %v", token, source, err)
	}
}

func TestBrowserLoginRefreshUpdatesEveryWorkspace(t *testing.T) {
	f := newBrowserLoginFixture(t)
	member, err := f.server.spaces.open(t.Context(), strings.Repeat("a", 64), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.server.spaces.close() })

	f.server.authMu.Lock()
	f.server.authToken = "old-token"
	f.server.authSource = "gh"
	f.server.authLogin = "old-user"
	f.server.authResolved = true
	f.server.authMu.Unlock()
	token, _, err := member.credentials(t.Context(), false)
	if err != nil || token != "old-token" {
		t.Fatalf("member initial token = %q, %v", token, err)
	}

	var lookups atomic.Int32
	f.server.authResolve = func(context.Context) (string, string, error) {
		lookups.Add(1)
		return "new-token", "gh", nil
	}
	f.server.login.run = func(_ context.Context, onCode func(string)) loginRunResult {
		onCode("ABCD-EFGH")
		return loginRunResult{}
	}
	f.request("POST", "/api/auth/login", map[string]any{}, http.StatusOK)
	succeeded := waitBrowserLogin(t, f, loginSucceeded)
	if succeeded.UserCode != "" || succeeded.VerificationURL != "" {
		t.Fatalf("success retained a device code: %+v", succeeded)
	}

	token, source, err := member.credentials(t.Context(), false)
	if err != nil || token != "new-token" || source != "gh" {
		t.Fatalf("member cached the old identity: %q %q %v", token, source, err)
	}
	if lookups.Load() != 1 {
		t.Fatalf("credential lookups = %d, want one refresh", lookups.Load())
	}
	auth := f.server.authenticationState()
	if auth["configured"] != true || auth["source"] != "gh" || auth["login"] != "" {
		t.Fatalf("stale login state after success: %+v", auth)
	}
}

func TestBrowserLoginBlockedByExplicitToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		apply  func(*testing.T, *fixture)
	}{
		{name: "GITHUB_TOKEN", source: "github_token", apply: func(_ *testing.T, f *fixture) {
			f.server.token = "sentinel-explicit-token"
		}},
		{name: "GH_TOKEN", source: "gh_token", apply: func(t *testing.T, _ *fixture) {
			t.Setenv("GH_TOKEN", "sentinel-explicit-token")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBrowserLoginFixture(t)
			tc.apply(t, f)
			var starts atomic.Int32
			f.server.login.run = func(ctx context.Context, _ func(string)) loginRunResult {
				starts.Add(1)
				<-ctx.Done()
				return loginRunResult{ExitCode: -1, Err: ctx.Err()}
			}
			out := f.request("POST", "/api/auth/login", map[string]any{}, http.StatusOK)
			state := decodeBrowserLogin(t, out)
			if state.Status != loginFailed || state.BlockedSource != tc.source || !state.Available {
				t.Fatalf("blocked state = %+v", state)
			}
			if !strings.Contains(state.Message, "restart TOUKO") {
				t.Fatalf("blocked message is not actionable: %s", state.Message)
			}
			if starts.Load() != 0 {
				t.Fatal("explicit credential started a gh login")
			}
			if strings.Contains(out.Body.String(), "sentinel-explicit-token") {
				t.Fatal("explicit credential reached the page")
			}
			passive := decodeBrowserLogin(t, f.request("GET", "/api/auth/login", nil, http.StatusOK))
			if passive.Status != loginFailed || passive.BlockedSource != tc.source {
				t.Fatalf("passive blocked state = %+v", passive)
			}
		})
	}
}

func TestBrowserLoginForbiddenDoesNotStartLogin(t *testing.T) {
	f := newBrowserLoginFixture(t)
	var starts atomic.Int32
	f.server.login.run = func(ctx context.Context, _ func(string)) loginRunResult {
		starts.Add(1)
		return loginRunResult{ExitCode: -1, Err: errors.New("must not start")}
	}
	f.server.authMu.Lock()
	f.server.authToken = "account-token"
	f.server.authSource = "gh"
	f.server.authResolved = true
	f.server.authMu.Unlock()
	branchAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/denied/branches" {
			t.Errorf("unexpected API path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	})

	out := f.request("GET", "/api/branches?repository=owner%2Fdenied", nil, http.StatusForbidden)
	var result struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error.Code != "forbidden" {
		t.Fatalf("permission failure code = %q, want forbidden", result.Error.Code)
	}
	state := decodeBrowserLogin(t, f.request("GET", "/api/auth/login", nil, http.StatusOK))
	if state.Status != loginIdle || starts.Load() != 0 {
		t.Fatalf("a GitHub 403 started a login flow: %+v (starts=%d)", state, starts.Load())
	}
}

func TestBrowserLoginMissingGHReportsInstallWithoutStarting(t *testing.T) {
	f := newBrowserLoginFixture(t)
	f.server.login.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	var starts atomic.Int32
	f.server.login.run = func(ctx context.Context, _ func(string)) loginRunResult {
		starts.Add(1)
		<-ctx.Done()
		return loginRunResult{ExitCode: -1, Err: ctx.Err()}
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		var out *httptest.ResponseRecorder
		if method == http.MethodGet {
			out = f.request("GET", "/api/auth/login", nil, http.StatusOK)
		} else {
			out = f.request("POST", "/api/auth/login", map[string]any{}, http.StatusOK)
		}
		state := decodeBrowserLogin(t, out)
		if state.Status != loginFailed || state.Available || state.BlockedSource != "" {
			t.Fatalf("%s missing-gh state = %+v", method, state)
		}
		if !strings.Contains(state.Message, ghInstallURL) {
			t.Fatalf("%s missing-gh message is not actionable: %s", method, state.Message)
		}
	}
	if starts.Load() != 0 {
		t.Fatal("missing gh started a login process")
	}
}

func TestBrowserLoginInstallUsesFixedPublicURL(t *testing.T) {
	f := newBrowserLoginFixture(t)
	var opened []string
	f.server.login.open = func(_ context.Context, target string) error {
		opened = append(opened, target)
		return nil
	}
	out := f.request("POST", "/api/auth/install", map[string]any{}, http.StatusOK)
	var result struct {
		Opened  bool   `json:"opened"`
		URL     string `json:"url"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Opened || result.URL != ghInstallURL || len(opened) != 1 || opened[0] != ghInstallURL {
		t.Fatalf("install opened %v, response %+v", opened, result)
	}

	rejected := browserLoginRequest(t, f, http.MethodPost, "/api/auth/install", `{"url":"https://evil.example"}`, testOrigin, f.csrf)
	if rejected.Code != http.StatusBadRequest || len(opened) != 1 {
		t.Fatalf("user-controlled install URL was accepted: %d %s", rejected.Code, rejected.Body.String())
	}
}
