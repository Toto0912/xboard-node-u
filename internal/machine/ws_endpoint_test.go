package machine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/gorilla/websocket"
)

func TestMachineMuxRetiresOnEndpointChangeAndRevocation(t *testing.T) {
	var path atomic.Value
	path.Store("/a")
	var reject atomic.Bool
	type socket struct {
		path string
		c    *websocket.Conn
	}
	opened := make(chan socket, 8)
	closed := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/machine/nodes":
			if reject.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, `{"nodes":[]}`)
		case "/api/v2/server/handshake":
			fmt.Fprintf(w, `{"websocket":{"enabled":true,"ws_url":"ws://%s%s"}}`, r.Host, path.Load())
		default:
			up := websocket.Upgrader{}
			c, e := up.Upgrade(w, r, nil)
			if e != nil {
				return
			}
			defer c.Close()
			c.WriteJSON(map[string]any{"event": "auth.success"})
			opened <- socket{r.URL.Path, c}
			for {
				if _, _, e := c.ReadMessage(); e != nil {
					closed <- r.URL.Path
					return
				}
			}
		}
	}))
	defer srv.Close()
	o := New(&config.Config{Panel: config.PanelConfig{URL: srv.URL}, Machine: &config.MachineConfig{MachineID: 1, Token: "fixture"}, WS: config.WSConfig{StatusInterval: 60, HandshakeTimeout: 5, DiscoveryInterval: 600}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer o.retireWS()
	waitOpen := func(path string) *websocket.Conn {
		t.Helper()
		select {
		case v := <-opened:
			if v.path != path {
				t.Fatal(v.path)
			}
			return v.c
		case <-time.After(3 * time.Second):
			t.Fatal("expected", path)
		}
		return nil
	}
	waitClose := func(path string) {
		t.Helper()
		select {
		case v := <-closed:
			if v != path {
				t.Fatal(v)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("expected closure", path)
		}
	}
	waitConnected := func() {
		t.Helper()
		until := time.Now().Add(time.Second)
		for {
			o.eventsMu.RLock()
			ok := o.wsConnected
			o.eventsMu.RUnlock()
			if ok {
				return
			}
			if time.Now().After(until) {
				t.Fatal("machine socket not yet authenticated")
			}
			time.Sleep(time.Millisecond)
		}
	}
	o.rediscover(ctx)
	waitOpen("/a")
	waitConnected()
	path.Store("/b")
	o.nextWSAttempt = time.Time{}
	o.rediscover(ctx)
	waitClose("/a")
	b := waitOpen("/b")
	waitConnected()
	reject.Store(true)
	if err := b.WriteJSON(map[string]any{"event": panel.WSEventMachineRevoked, "data": map[string]any{"code": http.StatusForbidden}}); err != nil {
		t.Fatal(err)
	}
	waitClose("/b")
	o.rediscover(ctx)
	if o.ws.Load() != nil {
		t.Fatal("rejected machine retained shared transport")
	}
	reject.Store(false)
	o.rediscover(ctx)
	waitOpen("/b")
	waitConnected()
}

func TestReconnectReconcilesMachineMembersChangedDuringOutage(t *testing.T) {
	var ports [4]int
	for i := 1; i <= 3; i++ {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	var unavailable, reassigned, revoked atomic.Bool
	sockets := make(chan *websocket.Conn, 12)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/machine/nodes":
			if revoked.Load() {
				w.WriteHeader(403)
				return
			}
			if unavailable.Load() {
				w.WriteHeader(503)
				return
			}
			first := 1
			if reassigned.Load() {
				first = 3
			}
			fmt.Fprintf(w, `{"nodes":[{"id":%d,"type":"vless"},{"id":2,"type":"vless"}]}`, first)
		case "/api/v2/server/handshake":
			fmt.Fprintf(w, `{"websocket":{"enabled":true,"ws_url":"ws://%s/ws"}}`, r.Host)
		case "/ws":
			if unavailable.Load() || revoked.Load() {
				w.WriteHeader(503)
				return
			}
			up := websocket.Upgrader{}
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			c.WriteJSON(map[string]any{"event": "auth.success"})
			sockets <- c
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		case "/api/v2/server/config":
			id, _ := strconv.Atoi(r.URL.Query().Get("node_id"))
			if id <= 0 || id > 3 {
				w.WriteHeader(400)
				return
			}
			fmt.Fprintf(w, `{"protocol":"vless","network":"tcp","listen_ip":"127.0.0.1","server_port":%d}`, ports[id])
		case "/api/v2/server/user":
			fmt.Fprint(w, `{"users":[{"id":1,"uuid":"11111111-1111-4111-8111-111111111111"}]}`)
		default:
			fmt.Fprint(w, "{}")
		}
	}))
	defer srv.Close()
	cfg := &config.Config{Panel: config.PanelConfig{URL: srv.URL}, Machine: &config.MachineConfig{MachineID: 2, Token: "fixture"}, Kernel: config.KernelConfig{Type: "xray", ConfigDir: t.TempDir(), LogLevel: "error"}, WS: config.WSConfig{DiscoveryInterval: 90, StatusInterval: 60, HandshakeTimeout: 2, BackoffInitial: 1, BackoffMax: 1}, Node: config.NodeConfig{TrackInterval: 30, DeviceReportInterval: 30}}
	o := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(5 * time.Second):
			t.Error("machine did not stop")
		}
	}()
	wait := func(want [3]bool) {
		t.Helper()
		end := time.Now().Add(7 * time.Second)
		for {
			var got [3]bool
			for i := 1; i <= 3; i++ {
				c, e := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", ports[i]), 150*time.Millisecond)
				if e == nil {
					got[i-1] = true
					c.Close()
				}
			}
			if got == want {
				return
			}
			if time.Now().After(end) {
				t.Fatalf("active members=%v, want %v", got, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	nextSocket := func() *websocket.Conn {
		t.Helper()
		select {
		case c := <-sockets:
			return c
		case <-time.After(5 * time.Second):
			t.Fatal("no machine socket")
		}
		return nil
	}
	a := nextSocket()
	wait([3]bool{true, true, false})
	unavailable.Store(true)
	a.Close()
	wait([3]bool{true, true, false})
	reassigned.Store(true)
	unavailable.Store(false)
	b := nextSocket()
	wait([3]bool{false, true, true})
	revoked.Store(true)
	b.WriteJSON(map[string]any{"event": panel.WSEventMachineRevoked, "data": map[string]any{"code": 403}})
	wait([3]bool{})
	revoked.Store(false)
	o.wakeDiscovery()
	wait([3]bool{false, true, true})
}
