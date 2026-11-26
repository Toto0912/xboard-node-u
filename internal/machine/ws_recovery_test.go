package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/gorilla/websocket"
)

func TestMachineWSRecoveryAttachesExistingRESTChild(t *testing.T) {
	for _, first := range []string{"disabled", "unavailable"} {
		t.Run(first, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			var enabled, removed atomic.Bool
			var handshakes, dials, fullPulls atomic.Int32
			connected := make(chan *websocket.Conn, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/server/machine/nodes":
					fmt.Fprint(w, `{"nodes":[{"id":7,"type":"vless"}]}`)
				case "/api/v2/server/handshake":
					var args struct {
						NodeID int `json:"node_id"`
					}
					_ = json.NewDecoder(r.Body).Decode(&args)
					if args.NodeID == 0 {
						handshakes.Add(1)
					}
					if !enabled.Load() {
						if first == "unavailable" {
							w.WriteHeader(503)
							return
						}
						fmt.Fprint(w, `{"websocket":{"enabled":false}}`)
						return
					}
					fmt.Fprintf(w, `{"websocket":{"enabled":true,"ws_url":"ws://%s/ws"}}`, r.Host)
				case "/ws":
					up := websocket.Upgrader{}
					conn, err := up.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					dials.Add(1)
					if err := conn.WriteJSON(map[string]any{"event": "auth.success"}); err != nil {
						return
					}
					connected <- conn
					for {
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
					}
				case "/api/v2/server/config":
					fmt.Fprintf(w, `{"protocol":"vless","listen_ip":"127.0.0.1","server_port":%d,"network":"tcp"}`, port)
				case "/api/v2/server/user":
					fullPulls.Add(1)
					if removed.Load() {
						fmt.Fprint(w, `{"users":[]}`)
						return
					}
					fmt.Fprint(w, `{"users":[{"id":1,"uuid":"11111111-1111-1111-1111-111111111111"}]}`)
				default:
					fmt.Fprint(w, "{}")
				}
			}))
			defer server.Close()
			cfg := &config.Config{Panel: config.PanelConfig{URL: server.URL}, Machine: &config.MachineConfig{MachineID: 1, Token: "test"},
				Kernel: config.KernelConfig{Type: "xray", ConfigDir: t.TempDir()}, Node: config.NodeConfig{TrackInterval: 60, PullInterval: 3600, DeviceReportInterval: 60},
				WS: config.WSConfig{DiscoveryInterval: 3600}}
			o := New(cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); o.stopChildren() }()
			wait := func(condition func() bool) {
				t.Helper()
				until := time.Now().Add(5 * time.Second)
				for !condition() {
					if time.Now().After(until) {
						t.Fatal("condition timed out")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			open := func() bool {
				c, e := net.DialTimeout("tcp4", addr, 100*time.Millisecond)
				if c != nil {
					c.Close()
				}
				return e == nil
			}
			o.rediscover(ctx)
			wait(open)
			firstChild := o.nodes[7]
			initial := fullPulls.Load()
			enabled.Store(true)
			o.rediscover(ctx)
			if handshakes.Load() != 1 || dials.Load() != 0 {
				t.Fatal("discovery burst bypassed retry interval")
			}
			o.nextWSAttempt = time.Now()
			o.rediscover(ctx)
			var conn *websocket.Conn
			select {
			case conn = <-connected:
			case <-time.After(5 * time.Second):
				t.Fatal("no shared WS recovery")
			}
			wait(func() bool { return fullPulls.Load() > initial })
			for i := 0; i < 3; i++ {
				o.rediscover(ctx)
			}
			if handshakes.Load() != 2 || dials.Load() != 1 || o.nodes[7] != firstChild {
				t.Fatal("duplicate mux or restarted child")
			}
			removed.Store(true)
			if err := conn.WriteJSON(map[string]any{"event": "sync.users", "data": map[string]any{"node_id": 7, "users": []any{}}}); err != nil {
				t.Fatal(err)
			}
			wait(func() bool { return !open() })
		})
	}
}
