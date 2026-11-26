package service

import (
	"context"
	"fmt"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoveryReplacesConnectedEndpointAndIsolatesRetiredFrames(t *testing.T) {
	var endpoint atomic.Value
	endpoint.Store("/a")
	var bad atomic.Bool
	accepted := make(chan string, 8)
	closed := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/handshake":
			if bad.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			path := endpoint.Load().(string)
			if path == "" {
				fmt.Fprint(w, `{"websocket":{"enabled":false}}`)
				return
			}
			fmt.Fprintf(w, `{"websocket":{"enabled":true,"ws_url":"ws://%s%s"}}`, r.Host, path)
		case "/a", "/b":
			up := websocket.Upgrader{}
			conn, e := up.Upgrade(w, r, nil)
			if e != nil {
				return
			}
			defer conn.Close()
			conn.WriteJSON(map[string]any{"event": "auth.success"})
			accepted <- r.URL.Path
			for {
				if _, _, e := conn.ReadMessage(); e != nil {
					closed <- r.URL.Path
					return
				}
			}
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer server.Close()
	cfg := &config.Config{Panel: config.PanelConfig{URL: server.URL, NodeID: 1}, WS: config.WSConfig{StatusInterval: 60, HandshakeTimeout: 5}}
	cp := controlplane.NewPanelControlPlane(cfg.Panel, cfg.WS, cfg.Kernel)
	s := newService(cfg, cp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expect := func(ch chan string, path string) {
		t.Helper()
		select {
		case got := <-ch:
			if got != path {
				t.Fatalf("path=%s want=%s", got, path)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("timeout waiting for", path)
		}
	}
	s.applyPushDiscovery(ctx, s.discoverPush(ctx))
	expect(accepted, "/a")
	old := s.wsClient
	events, status := s.wsEvents, s.wsStatus
	s.applyPushDiscovery(ctx, s.discoverPush(ctx))
	if s.wsClient != old {
		t.Fatal("same endpoint reconnected")
	}
	bad.Store(true)
	s.applyPushDiscovery(ctx, s.discoverPush(ctx))
	if s.wsClient != old {
		t.Fatal("outage retired valid transport")
	}
	bad.Store(false)
	endpoint.Store("/b")
	s.applyPushDiscovery(ctx, s.discoverPush(ctx))
	expect(closed, "/a")
	expect(accepted, "/b")
	events <- controlplane.Event{Type: controlplane.EventSyncUsers}
	status.Publish(controlplane.StatusChange{Connected: false})
	select {
	case <-s.wsEvents:
		t.Fatal("retired user update visible")
	default:
	}
	if s.wsStatus == status || s.wsClient == old {
		t.Fatal("retired generation reused")
	}
	endpoint.Store("")
	s.applyPushDiscovery(ctx, s.discoverPush(ctx))
	expect(closed, "/b")
	if s.wsClient != nil {
		t.Fatal("disabled endpoint still installed")
	}
}
