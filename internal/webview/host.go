package webview

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/app"
)

// Host owns the listener and requests, but borrows the application Service.
// Call Close before closing the Service, including after Wait reports an error.
type Host struct {
	URL         string
	server      *http.Server
	cancel      context.CancelFunc
	done        chan struct{}
	serveErr    error
	connections sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
	workspaces  *workspaceSet
}

// Listen starts the same secure loopback surface for browser and MyGo windows.
func Listen(ctx context.Context, service *app.Service, token string, port int) (*Host, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("port must be between 0 and 65535")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	origin := "http://" + listener.Addr().String()
	handler, err := New(service, token, origin)
	if err != nil {
		listener.Close()
		return nil, err
	}
	requests, cancel := context.WithCancel(ctx)
	host := &Host{URL: origin, cancel: cancel, done: make(chan struct{}), workspaces: handler.spaces}
	host.server = &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return requests },
		ConnState: func(_ net.Conn, state http.ConnState) {
			// StateNew precedes the connection goroutine. StateClosed follows
			// the handler's exit, even after a forced server.Close.
			switch state {
			case http.StateNew:
				host.connections.Add(1)
			case http.StateClosed, http.StateHijacked:
				host.connections.Done()
			}
		},
	}
	go func() {
		host.serveErr = host.server.Serve(listener)
		if errors.Is(host.serveErr, http.ErrServerClosed) {
			host.serveErr = nil
		}
		close(host.done)
	}()
	return host, nil
}

// Wait returns on cancellation or listener failure. Cancellation is a normal
// request to stop serving; Close still joins all in-flight request handlers.
func (h *Host) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-h.done:
		return h.serveErr
	}
}

func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		h.cancel()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.closeErr = h.server.Shutdown(shutdown)
		if h.closeErr != nil {
			_ = h.server.Close()
		}
		// Serve cannot deliver another StateNew after it has returned.
		<-h.done
		h.connections.Wait()
		h.closeErr = errors.Join(h.closeErr, h.serveErr)
		h.closeErr = errors.Join(h.closeErr, h.workspaces.close())
	})
	return h.closeErr
}
