// Package desktop owns the MyGo window, not a separate gallery state machine.
package desktop

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sync"
	"sync/atomic"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/browser"
	"github.com/Eurekaimer/TOUKO/internal/webview"
	"github.com/egoist/mygo"
)

// The icon ladder in assets/ is derived from the author's artwork by
// packaging/make-icons.py; the original logo file is not part of the build.
// AppIndicator needs a small square image, while compositors that read the
// window icon get the larger one.
//
//go:embed assets/touko-64.png
var trayIcon []byte

//go:embed assets/touko-256.png
var windowIcon []byte

// The tray and its menu must stay reachable for as long as the process runs:
// MyGo resolves a clicked item through weak references, so a collected menu
// would leave every tray entry visibly present and silently dead.
var (
	trayMu      sync.Mutex
	trayHandle  *mygo.Tray
	trayMenuRef *mygo.Menu
)

// session is the running app the tray menu acts on.
type session struct {
	window   *mygo.Window
	loopback string
	stateDir string
	output   io.Writer
	// quitting tells the close handler to let the window close: MyGo abandons
	// the whole quit when any window refuses, so hiding must stop once the user
	// has actually asked to leave.
	quitting *atomic.Bool
}

// Run must run on the main goroutine: MyGo drives the OS webview on that thread.
// It opens the same embedded UI and API as serve, keeping all Git and token IO
// in Go. Closing the window hides it into the tray while the loopback server
// keeps running; quitting — from the tray menu, the window manager, or a signal
// — cancels and joins requests before closing SQLite.
func Run(stateDir string, port int, output io.Writer) (err error) {
	// MyGo owns OS signal handling and its native quit event loop.
	ctx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	if !mygo.WebViewAvailable() {
		return fmt.Errorf("MyGo requires the system webview: GTK 3 and WebKitGTK 4.1 on Linux, WebView2 on Windows, or WKWebView on macOS")
	}
	dir, err := app.StateDir(stateDir)
	if err != nil {
		return err
	}
	service, err := app.Open(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, service.Close()) }()
	host, err := webview.Listen(ctx, service, os.Getenv("GITHUB_TOKEN"), port)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, host.Close()) }()
	if _, err = fmt.Fprintf(output, "TOUKO MyGo: %s\nState: %s\n", host.URL, dir); err != nil {
		return err
	}

	mygo.App.SetName("TOUKO")
	// The capability protocol must exist before the window that fetches it.
	clipboard, err := newClipboardBridge(host.URL)
	if err != nil {
		return err
	}
	defer clipboard.close()
	watch, cancel := context.WithCancel(ctx)
	var quitting atomic.Bool
	var watcher sync.WaitGroup
	defer func() { cancel(); watcher.Wait() }()
	defer mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) {
		quitting.Store(true)
		cancel()
	})()
	mygo.App.WhenReady(func() {
		window := openWindow(host.URL)
		if iconErr := window.SetIcon(windowIcon); iconErr != nil {
			fmt.Fprintf(output, "Window icon unavailable: %v\n", iconErr)
		}
		mygo.BindAs("DesktopClipboard", clipboard)
		if attachErr := clipboard.attach(window, window.Page()); attachErr != nil {
			fmt.Fprintf(output, "Clipboard paste unavailable: %v\n", attachErr)
		}
		ready := session{window: window, loopback: host.URL, stateDir: dir, output: output, quitting: &quitting}
		// Hiding is only offered when the tray can bring the window back. Without
		// a tray host the window keeps ending the process, so the app can never
		// end up running with no way to reach it.
		if trayErr := ready.addTray(); trayErr != nil {
			fmt.Fprintf(output, "Tray icon unavailable (%v); closing the window stops TOUKO.\n", trayErr)
		} else {
			fmt.Fprintf(output, "Closing the window hides it; reopen or quit from the tray icon.\n")
			window.OnClose(ready.onClose)
		}
		watcher.Go(func() {
			if host.Wait(watch) != nil {
				mygo.App.Quit()
			}
		})
	})
	return mygo.App.Run()
}

// onClose turns a window close into a hide, unless the app is already leaving.
func (s session) onClose(event *mygo.CloseEvent) {
	if s.quitting.Load() {
		return
	}
	event.PreventDefault()
	s.window.Hide()
	fmt.Fprintf(s.output, "Window hidden; still serving %s.\n", s.loopback)
}

// addTray installs the notification-area icon. AppIndicator opens the menu on
// click and reports no click event of its own, so reopening the window has to
// be one of the menu items.
func (s session) addTray() error {
	// Failures are reported on stdout: the window may be hidden when an item is
	// chosen, so a silent no-op would look like a broken menu.
	launch := func(what, target string) func(*mygo.MenuItem, *mygo.Window) {
		return func(*mygo.MenuItem, *mygo.Window) {
			if err := browser.Open(context.Background(), target); err != nil {
				fmt.Fprintf(s.output, "Could not open %s: %v\n", what, err)
			}
		}
	}
	menu := mygo.NewMenu([]*mygo.MenuItem{
		{ID: "show", Label: "显示主窗口", Click: func(*mygo.MenuItem, *mygo.Window) {
			s.window.Show()
			s.window.Focus()
		}},
		{ID: "open-browser", Label: "在浏览器中打开", Click: launch("the browser", s.loopback)},
		{ID: "open-state", Label: "打开工作区目录", Click: launch("the workspace directory", s.stateDir)},
		{Type: mygo.MenuItemSeparator},
		{ID: "quit", Label: "退出 TOUKO", Click: func(*mygo.MenuItem, *mygo.Window) {
			fmt.Fprintf(s.output, "Quitting from the tray icon.\n")
			s.quitting.Store(true)
			mygo.App.Quit()
		}},
	})
	tray, err := mygo.NewTray(mygo.TrayOptions{Icon: trayIcon, ToolTip: "TOUKO 图片库", Menu: menu})
	if err != nil {
		return err
	}
	// A single click opens the window where the platform reports tray clicks
	// (macOS menu bar, Windows notification area). On Linux the indicator host
	// shows the menu instead — AppIndicator reports no click event — so there the
	// first item is the way back to the window.
	tray.OnClick(func() {
		s.window.Show()
		s.window.Focus()
	})
	trayMu.Lock()
	defer trayMu.Unlock()
	trayHandle, trayMenuRef = tray, menu
	return nil
}

func openWindow(origin string) *mygo.Window {
	window := mygo.NewWindow(mygo.WindowOptions{
		Title: "TOUKO", URL: origin, Width: 1440, Height: 960,
		MinWidth: 390, MinHeight: 600, BackgroundColor: "#fbfaf7",
		// MyGo refuses Go method calls from an origin it does not trust, so the
		// clipboard bridge would silently never appear on the page.
		Page: mygo.PageOptions{DevTools: mygo.DevToolsDisabled, TrustedOrigins: []string{origin}},
	})
	page := window.Page()
	page.OnWillNavigate(func(event *mygo.NavigateEvent) {
		target, err := url.Parse(event.URL)
		if err != nil || target.Scheme+"://"+target.Host != origin {
			event.PreventDefault()
		}
	})
	page.SetWindowOpenHandler(func(mygo.WindowOpenRequest) *mygo.WindowOptions { return nil })
	page.SetPermissionHandler(func(mygo.PermissionRequest) bool { return false })
	return window
}
