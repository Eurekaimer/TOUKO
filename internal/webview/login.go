package webview

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/browser"
)

const (
	loginIdle      = "idle"
	loginStarting  = "starting"
	loginWaiting   = "waiting"
	loginSucceeded = "succeeded"
	loginFailed    = "failed"
	loginCancelled = "cancelled"

	githubDeviceURL = "https://github.com/login/device"
	ghInstallURL    = "https://cli.github.com/"
	loginLifetime   = 15 * time.Minute
	browserLifetime = 15 * time.Second
)

const plaintextWarning = "GitHub CLI warned that it could not use the system keyring and may have stored the token in a plaintext file; secure or remove that file."

var deviceCodePattern = regexp.MustCompile(`(?i)one-time code:\s*([A-Z0-9]{4}-[A-Z0-9]{4})`)
var deviceURLPattern = regexp.MustCompile(`https://github\.com/login/device`)
var plaintextPattern = regexp.MustCompile(`(?i)plain\s*text`)

// loginState is the only login shape exposed to pages. user_code and
// verification_url exist only while gh is waiting for the browser flow, so a
// terminal failure cannot leave a stale code on screen.
type loginState struct {
	Status          string `json:"status"`
	UserCode        string `json:"user_code,omitempty"`
	VerificationURL string `json:"verification_url,omitempty"`
	Message         string `json:"message"`
	Available       bool   `json:"available"`
	BlockedSource   string `json:"blocked_source"`
	CSRF            string `json:"csrf"`
}

type loginRunResult struct {
	ExitCode  int
	Plaintext bool
	Err       error
}

// loginOutput accepts only the two values gh's device flow prints: a
// four-by-four code after the one-time-code label and the fixed github.com
// device URL. Nothing else from the child's output is retained.
type loginOutput struct {
	code      string
	url       bool
	notified  bool
	plaintext bool
}

func (o *loginOutput) parse(line string) (string, bool) {
	if plaintextPattern.MatchString(line) {
		o.plaintext = true
	}
	if match := deviceCodePattern.FindStringSubmatch(line); match != nil {
		o.code = strings.ToUpper(match[1])
	}
	if deviceURLPattern.MatchString(line) {
		o.url = true
	}
	if o.code != "" && o.url && !o.notified {
		o.notified = true
		return o.code, true
	}
	return "", false
}

// loginRunner owns one gh process. Tests replace it to exercise the state
// machine without a network, keyring, or real browser.
type loginRunner func(context.Context, func(code string)) loginRunResult

type loginManager struct {
	mu         sync.Mutex
	status     string
	userCode   string
	message    string
	cancelled  bool
	closed     bool
	generation uint64
	cancelRun  context.CancelFunc
	done       chan struct{}
	run        loginRunner
	open       func(context.Context, string) error
	lookPath   func(string) (string, error)
	lifetime   time.Duration
}

func newLoginManager() *loginManager {
	return &loginManager{
		status:   loginIdle,
		run:      runGHLogin,
		open:     browser.Open,
		lookPath: exec.LookPath,
		lifetime: loginLifetime,
	}
}

func isLoginEndpoint(path string) bool {
	switch path {
	case "/api/auth/login", "/api/auth/login/cancel", "/api/auth/login/open", "/api/auth/install":
		return true
	}
	return false
}

func (m *loginManager) snapshot() loginState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *loginManager) snapshotLocked() loginState {
	state := loginState{Status: m.status, Message: m.message}
	if m.status == loginWaiting && m.userCode != "" {
		state.UserCode = m.userCode
		state.VerificationURL = githubDeviceURL
	}
	return state
}

func (m *loginManager) available() bool {
	m.mu.Lock()
	lookPath := m.lookPath
	m.mu.Unlock()
	if lookPath == nil {
		return false
	}
	_, err := lookPath("gh")
	return err == nil
}

func (m *loginManager) opener() func(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.open
}

func (s *Server) loginBlockedSource() string {
	owner := s.authOwner()
	if owner.token != "" || os.Getenv("GITHUB_TOKEN") != "" {
		return "github_token"
	}
	if os.Getenv("GH_TOKEN") != "" {
		return "gh_token"
	}
	return ""
}

func loginBlockedMessage(source string) string {
	name := "GITHUB_TOKEN"
	if source == "gh_token" {
		name = "GH_TOKEN"
	}
	return name + " is set, so browser sign-in is disabled. Remove it and restart TOUKO to sign in with GitHub CLI."
}

// loginStateResponse answers without starting gh, opening a browser, or taking
// the gallery work lock.
func (s *Server) loginStateResponse() loginState {
	owner := s.authOwner()
	state := owner.login.snapshot()
	state.CSRF = owner.csrf
	state.Available = owner.login.available()
	state.BlockedSource = owner.loginBlockedSource()
	if state.BlockedSource != "" {
		state.Status = loginFailed
		state.UserCode = ""
		state.VerificationURL = ""
		state.Message = loginBlockedMessage(state.BlockedSource)
	} else if !state.Available && state.Status == loginIdle {
		state.Status = loginFailed
		state.Message = "GitHub CLI (gh) was not found. Install it from " + ghInstallURL + ", then try again."
	}
	return state
}

func (s *Server) startLogin() loginState {
	state := s.loginStateResponse()
	if state.BlockedSource != "" || !state.Available {
		return state
	}
	owner := s.authOwner()
	owner.login.start(owner)
	return s.loginStateResponse()
}

func (s *Server) cancelLogin() loginState {
	owner := s.authOwner()
	owner.login.cancel()
	return s.loginStateResponse()
}

func (s *Server) openLoginDevice(ctx context.Context) (loginState, error) {
	owner := s.authOwner()
	if err := owner.login.openDevice(ctx); err != nil {
		return loginState{}, err
	}
	return s.loginStateResponse(), nil
}

func (s *Server) openGHInstall(ctx context.Context) (bool, string) {
	return s.authOwner().login.openInstall(ctx)
}

func (s *Server) shutdownLogin() {
	s.authOwner().login.shutdown()
}

func (m *loginManager) start(owner *Server) loginState {
	m.mu.Lock()
	if m.closed || m.status == loginStarting || m.status == loginWaiting {
		state := m.snapshotLocked()
		m.mu.Unlock()
		return state
	}
	lifetime := m.lifetime
	if lifetime <= 0 {
		lifetime = loginLifetime
	}
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	done := make(chan struct{})
	run := m.run
	m.generation++
	generation := m.generation
	m.status = loginStarting
	m.userCode = ""
	m.message = "Starting GitHub sign-in with the GitHub CLI. You can cancel while waiting; sign-in will time out after 15 minutes."
	m.cancelled = false
	m.cancelRun = cancel
	m.done = done
	m.mu.Unlock()

	go m.session(ctx, cancel, done, owner, run, generation)
	return m.snapshot()
}

func (m *loginManager) session(ctx context.Context, cancel context.CancelFunc, done chan struct{}, owner *Server, run loginRunner, generation uint64) {
	// LIFO: cancel the process context before signaling joiners that this
	// goroutine has finished all cleanup.
	defer close(done)
	defer cancel()

	result := loginRunResult{ExitCode: -1, Err: errors.New("login runner is unavailable")}
	if run != nil {
		result = run(ctx, func(code string) { m.deviceCode(ctx, code, generation) })
	}
	if m.stale(generation) {
		return
	}
	if ctx.Err() == context.DeadlineExceeded {
		m.finish(generation, loginFailed, "GitHub sign-in timed out. Try again.", false)
		return
	}
	if ctx.Err() == context.Canceled {
		m.finish(generation, loginCancelled, "GitHub sign-in was cancelled.", false)
		return
	}
	if result.Err != nil {
		m.finish(generation, loginFailed, loginFailureMessage(result), result.Plaintext)
		return
	}

	m.mu.Lock()
	if m.generation != generation || m.cancelled {
		m.mu.Unlock()
		return
	}
	m.userCode = ""
	m.status = loginStarting
	m.message = "GitHub CLI finished. Refreshing TOUKO credentials…"
	m.mu.Unlock()

	if err := owner.refreshAuth(ctx); err != nil {
		if m.stale(generation) || ctx.Err() == context.Canceled {
			return
		}
		if ctx.Err() == context.DeadlineExceeded {
			m.finish(generation, loginFailed, "GitHub sign-in timed out. Try again.", false)
			return
		}
		m.finish(generation, loginFailed, "GitHub CLI finished, but TOUKO could not read the stored credential. Run `gh auth status` for details, then try again.", result.Plaintext)
		return
	}
	if m.stale(generation) || ctx.Err() == context.Canceled {
		return
	}
	if ctx.Err() == context.DeadlineExceeded {
		m.finish(generation, loginFailed, "GitHub sign-in timed out. Try again.", false)
		return
	}
	m.finish(generation, loginSucceeded, "Signed in to GitHub.", result.Plaintext)
}

func loginFailureMessage(result loginRunResult) string {
	message := "GitHub CLI sign-in failed"
	if result.ExitCode > 0 {
		message += " (exit status " + strconv.Itoa(result.ExitCode) + ")"
	}
	return message + ". Run `gh auth status` for details, then try again."
}

// stale reports whether a session was cancelled or superseded, so late
// callbacks from an old gh process cannot drive a newly started session.
func (m *loginManager) stale(generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation != generation || m.cancelled
}

func (m *loginManager) deviceCode(ctx context.Context, code string, generation uint64) {
	m.mu.Lock()
	if m.generation != generation || m.status != loginStarting || m.cancelled || m.userCode != "" {
		m.mu.Unlock()
		return
	}
	m.status = loginWaiting
	m.userCode = code
	m.message = "Enter this one-time code at " + githubDeviceURL + " to finish signing in."
	open := m.open
	m.mu.Unlock()

	if open == nil || ctx.Err() != nil {
		return
	}
	openContext, cancel := context.WithTimeout(ctx, browserLifetime)
	defer cancel()
	if err := open(openContext, githubDeviceURL); err != nil {
		m.mu.Lock()
		if m.status == loginWaiting {
			m.message = "Could not open your browser. Open " + githubDeviceURL + " and enter the code shown here."
		}
		m.mu.Unlock()
	}
}

func (m *loginManager) finish(generation uint64, status, message string, plaintext bool) {
	if plaintext {
		message += " " + plaintextWarning
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != generation || m.cancelled {
		return
	}
	m.status = status
	m.userCode = ""
	m.message = message
}

func (m *loginManager) cancel() {
	m.mu.Lock()
	if m.status != loginStarting && m.status != loginWaiting {
		m.mu.Unlock()
		return
	}
	m.cancelled = true
	m.status = loginCancelled
	m.userCode = ""
	m.message = "GitHub sign-in was cancelled."
	cancel, done := m.cancelRun, m.done
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (m *loginManager) shutdown() {
	m.mu.Lock()
	m.closed = true
	cancel, done := m.cancelRun, m.done
	if m.status == loginStarting || m.status == loginWaiting {
		m.cancelled = true
		m.status = loginCancelled
		m.userCode = ""
		m.message = "GitHub sign-in was cancelled because TOUKO stopped."
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (m *loginManager) openDevice(parent context.Context) error {
	m.mu.Lock()
	if m.status != loginWaiting {
		m.mu.Unlock()
		return usage("GitHub sign-in is not waiting for a code")
	}
	open := m.open
	m.mu.Unlock()
	if open == nil {
		return usage("no browser launcher is available")
	}
	ctx, cancel := context.WithTimeout(parent, browserLifetime)
	defer cancel()
	if err := open(ctx, githubDeviceURL); err != nil {
		m.mu.Lock()
		if m.status == loginWaiting {
			m.message = "Could not open your browser. Open " + githubDeviceURL + " and enter the code shown here."
		}
		m.mu.Unlock()
		return nil
	}
	m.mu.Lock()
	if m.status == loginWaiting {
		m.message = "Enter this one-time code at " + githubDeviceURL + " to finish signing in."
	}
	m.mu.Unlock()
	return nil
}

func (m *loginManager) openInstall(parent context.Context) (bool, string) {
	open := m.opener()
	if open == nil {
		return false, "Could not open your browser. Open " + ghInstallURL + " manually to install GitHub CLI."
	}
	ctx, cancel := context.WithTimeout(parent, browserLifetime)
	defer cancel()
	if err := open(ctx, ghInstallURL); err != nil {
		return false, "Could not open your browser. Open " + ghInstallURL + " manually to install GitHub CLI."
	}
	return true, "Opened the GitHub CLI installation page in your browser."
}

// runGHLogin drives the real CLI device flow. gh is run without a TTY, so it
// prints the one-time code and the fixed github.com device URL instead of
// opening a browser; TOUKO opens only that validated URL. Raw stderr is never
// returned to a page.
func runGHLogin(ctx context.Context, onCode func(string)) loginRunResult {
	cmd := exec.CommandContext(ctx, "gh", "auth", "login", "--hostname", "github.com", "--web", "--git-protocol", "https")
	cmd.Env = loginEnvironment(os.Environ())
	cmd.Stdin = nil
	configureLoginCommand(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return loginRunResult{ExitCode: -1, Err: err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return loginRunResult{ExitCode: -1, Err: err}
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return loginRunResult{ExitCode: -1, Err: err}
	}

	result := loginRunResult{}
	// gh writes user-facing messages to stderr, but a wrapper may use stdout.
	// Both streams are parsed for only the fixed device code and URL, and
	// neither is ever returned to a page.
	var mu sync.Mutex
	output := &loginOutput{}
	parse := func(line string) {
		mu.Lock()
		code, notify := output.parse(line)
		mu.Unlock()
		if notify && onCode != nil {
			onCode(code)
		}
	}
	scan := func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 256<<10)
		for scanner.Scan() {
			parse(scanner.Text())
		}
		if scanner.Err() != nil {
			_, _ = io.Copy(io.Discard, reader)
		}
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		scan(stdout)
	}()
	go func() {
		defer readers.Done()
		scan(stderr)
	}()
	readers.Wait()
	result.Plaintext = output.plaintext

	err = cmd.Wait()
	if err == nil {
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
		return result
	}
	result.Err = err
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
	} else if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	} else {
		result.ExitCode = -1
	}
	return result
}

func loginEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		name := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			name = entry[:index]
		}
		if strings.EqualFold(name, "NO_COLOR") || strings.EqualFold(name, "GH_PROMPT_DISABLED") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "NO_COLOR=1", "GH_PROMPT_DISABLED=1")
}
