// Package desktop owns the MyGo window, not a separate gallery state machine.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sync"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/webview"
	"github.com/egoist/mygo"
)

// Run must run on the main goroutine: MyGo drives the OS webview on that thread.
// It opens the same embedded UI and API as serve, keeping all Git and token IO
// in Go. Closing the window cancels and joins requests before closing SQLite.
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
	if _, err = fmt.Fprintf(output, "TOUKO MyGo: %s\nState: %s\nClose the window to stop.\n", host.URL, dir); err != nil {
		return err
	}

	mygo.App.SetName("TOUKO")
	watch, cancel := context.WithCancel(ctx)
	var watcher sync.WaitGroup
	defer func() { cancel(); watcher.Wait() }()
	defer mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { cancel() })()
	mygo.App.WhenReady(func() {
		openWindow(host.URL)
		watcher.Go(func() {
			if host.Wait(watch) != nil {
				mygo.App.Quit()
			}
		})
	})
	return mygo.App.Run()
}

func openWindow(origin string) *mygo.Window {
	window := mygo.NewWindow(mygo.WindowOptions{
		Title: "TOUKO", URL: origin, Width: 1440, Height: 960,
		MinWidth: 390, MinHeight: 600, BackgroundColor: "#fbfaf7",
		Page: mygo.PageOptions{DevTools: mygo.DevToolsDisabled},
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
