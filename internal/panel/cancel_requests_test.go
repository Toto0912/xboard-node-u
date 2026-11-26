package panel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
)

func TestControlRequestsEndWhenNodeStops(t *testing.T) {
	calls := map[string]func(context.Context, *Client) error{
		"handshake":          func(ctx context.Context, c *Client) error { _, e := c.HandshakeContext(ctx); return e },
		"node config":        func(ctx context.Context, c *Client) error { _, _, e := c.GetConfigContext(ctx); return e },
		"node users":         func(ctx context.Context, c *Client) error { _, _, e := c.GetUsersContext(ctx); return e },
		"machine membership": func(ctx context.Context, c *Client) error { _, e := c.GetMachineNodesContext(ctx); return e },
		"machine status": func(ctx context.Context, c *Client) error {
			return c.ReportMachineStatusContext(ctx, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, 0, 0)
		},
	}
	for name, request := range calls {
		t.Run(name, func(t *testing.T) {
			active, closed := make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(active)
				<-r.Context().Done()
				close(closed)
			}))
			defer srv.Close()
			c := NewClient(config.PanelConfig{URL: srv.URL, MachineID: 1})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- request(ctx, c) }()
			select {
			case <-active:
			case <-time.After(time.Second):
				t.Fatal("request never reached panel")
			}
			cancel()
			select {
			case e := <-done:
				if !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("request remained active")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("panel still has active connection")
			}
		})
	}
}
