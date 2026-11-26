package machine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
)

// Check the normal machine discovery, Service and Xray lifecycle, including
// recovery with the same credential after the machine is enabled again.
func TestMachineDiscoveryRevokesAndRecoversListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	var status atomic.Int32
	status.Store(http.StatusOK)
	const assigned = `{"nodes":[{"id":7,"type":"vless"}]}`
	var body atomic.Value
	body.Store(assigned)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/machine/nodes":
			if status.Load() == 0 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			w.WriteHeader(int(status.Load()))
			fmt.Fprint(w, body.Load())
		case "/api/v2/server/handshake":
			fmt.Fprint(w, `{"websocket":{"enabled":false}}`)
		case "/api/v2/server/config":
			fmt.Fprintf(w, `{"protocol":"vless","listen_ip":"127.0.0.1","server_port":%d,"network":"tcp"}`, port)
		case "/api/v2/server/user":
			fmt.Fprint(w, `{"users":[{"id":1,"uuid":"11111111-1111-1111-1111-111111111111"}]}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()
	o := New(&config.Config{
		Panel:   config.PanelConfig{URL: srv.URL},
		Machine: &config.MachineConfig{MachineID: 1, Token: "fixture"},
		Kernel:  config.KernelConfig{Type: "xray", ConfigDir: t.TempDir(), LogLevel: "error"},
		Node:    config.NodeConfig{TrackInterval: 60, DeviceReportInterval: 60},
		WS:      config.WSConfig{DiscoveryInterval: 60},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer o.stopChildren()
	defer cancel()
	checkPort := func(open bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.DialTimeout("tcp4", addr, 100*time.Millisecond)
			if conn != nil {
				conn.Close()
			}
			if (err == nil) == open {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("listener %s did not reach open=%v", addr, open)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	o.rediscover(ctx)
	checkPort(true)
	for _, invalid := range []string{`{}`, `{"nodes":null}`, `{"nodes":[{}]}`, "invalid json"} {
		body.Store(invalid)
		o.rediscover(ctx)
		checkPort(true)
	}
	body.Store(assigned)
	status.Store(0)
	o.rediscover(ctx)
	checkPort(true)
	status.Store(http.StatusServiceUnavailable)
	o.rediscover(ctx)
	checkPort(true)
	for _, rejected := range []int32{http.StatusUnauthorized, http.StatusForbidden} {
		status.Store(rejected)
		o.rediscover(ctx)
		checkPort(false)
		status.Store(http.StatusOK)
		o.rediscover(ctx)
		checkPort(true)
	}
	body.Store(`{"nodes":[]}`)
	o.rediscover(ctx)
	checkPort(false)
}

func TestMachineRevokedDuringDiscoveryDiscardsLateSuccess(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls, nodeStarts atomic.Int32
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/machine/nodes":
			switch calls.Add(1) {
			case 1:
				fmt.Fprint(w, `{"nodes":[{"id":7,"type":"vless"}]}`)
			case 2:
				close(started)
				<-release
				fmt.Fprint(w, `{"nodes":[{"id":8,"type":"vless"}]}`)
			default:
				w.WriteHeader(http.StatusForbidden)
			}
		case "/api/v2/server/handshake":
			fmt.Fprint(w, `{}`)
		default:
			nodeStarts.Add(1)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	childCtx, stop := context.WithCancel(ctx)
	o := New(&config.Config{Panel: config.PanelConfig{URL: srv.URL}, Machine: &config.MachineConfig{MachineID: 1, Token: "fixture"}})
	exited := make(chan struct{})
	o.nodes[7] = &nodeHandle{done: exited, cancel: stop}
	go func() { <-childCtx.Done(); close(exited) }()
	o.rediscover(ctx)
	done := make(chan struct{})
	o.onWSEvent(panel.WSEvent{Type: panel.WSEventSyncNodes})
	<-o.discoveryWake
	go func() { defer close(done); o.rediscover(ctx) }()
	defer func() { cancel(); unblock(); <-done }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("discovery did not reach the HTTP endpoint")
	}
	for i := 0; i < 100; i++ {
		o.onWSEvent(panel.WSEvent{Type: panel.WSEventSyncNodes})
	}
	o.onWSEvent(panel.WSEvent{Type: panel.WSEventMachineRevoked})
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("revocation waited for the outstanding HTTP request")
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("discovery did not finish")
	}
	if len(o.discoveryWake) != 1 || calls.Load() != 2 || nodeStarts.Load() != 0 {
		t.Fatalf("pending=%d HTTP calls=%d stale child requests=%d", len(o.discoveryWake), calls.Load(), nodeStarts.Load())
	}
	<-o.discoveryWake
	o.rediscover(ctx)
	if len(o.nodes) != 0 || calls.Load() != 3 {
		t.Fatalf("remaining nodes=%d HTTP calls=%d", len(o.nodes), calls.Load())
	}
}
