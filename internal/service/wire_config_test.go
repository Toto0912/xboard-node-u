package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/gorilla/websocket"
)

// Exercise both decode boundaries on a live VLESS listener. The old user
// remains reachable during a network failure, then loses access on revocation.
func verifyWireConfigProtection(t *testing.T, s *Service, nc *model.NodeSpec, a, b model.UserSpec, open func(string, string) net.Conn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := model.UserSpec{ID: 3, UUID: "33333333-3333-4333-8333-333333333333"}
	check := func(u model.UserSpec, allowed bool) {
		t.Helper()
		conn := open(u.UUID, "127.0.0.1")
		if conn != nil {
			conn.Close()
		}
		if (conn != nil) != allowed {
			t.Fatalf("user %d: expected access=%t", u.ID, allowed)
		}
	}
	invalid := `{"protocol":"vless","cert_config":{"http_port":{"invalid":true}}}`
	var phase, userRequests atomic.Int32
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			switch phase.Load() {
			case 0:
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			case 1:
				w.Header().Set("ETag", "invalid")
				fmt.Fprint(w, invalid)
				return
			}
			if r.Header.Get("If-None-Match") == "valid" {
				w.WriteHeader(304)
				return
			}
			w.Header().Set("ETag", "valid")
			json.NewEncoder(w).Encode(panel.NodeConfig{Protocol: nc.Protocol, ListenIP: nc.ListenIP, ServerPort: nc.ServerPort, Network: nc.Network, CustomRoutes: nc.CustomRoutes})
			return
		}
		userRequests.Add(1)
		if r.Header.Get("If-None-Match") == "next" {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", "next")
		json.NewEncoder(w).Encode(panel.UsersResponse{Users: []panel.User{{ID: a.ID, UUID: a.UUID, SpeedLimit: 2, DeviceLimit: 1}, {ID: b.ID, UUID: b.UUID}}})
	}))
	defer rest.Close()
	pc := config.PanelConfig{URL: rest.URL, NodeID: 7, Token: "fixture"}
	if s.cfg.Kernel.Type == "xray" {
		pc.MachineID = 12
	}
	originalSource := s.source
	defer func() { s.source = originalSource }()
	if pc.MachineID == 0 {
		s.source = controlplane.NewPanelControlPlane(pc, config.WSConfig{}, s.cfg.Kernel)
	} else {
		s.source = controlplane.NewMachinePanelControlPlane(panel.NewClient(pc), s.cfg.Kernel, nil, nil)
	}
	startUsers := []model.UserSpec{a, old}
	startUsers[0].DeviceLimit = 4
	if !s.applyUserUpdate(ctx, startUsers, computeUserHash(startUsers)) {
		t.Fatal("cannot stage old roster")
	}
	if snap, err := s.source.Poll(ctx); err == nil || snap.ConfigPending || userRequests.Load() != 0 {
		t.Fatal("outage classified as a new configuration")
	}
	check(old, true)
	phase.Store(1)
	// Single-node REST must work without any WS message. The machine goes through
	// the WS decoder first, independently of retrieval of the authoritative list.
	if pc.MachineID != 0 {
		events := make(chan panel.WSEvent, 1)
		upgrader := websocket.Upgrader{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"auth.success"}`))
			conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"sync.config","data":{"node_id":7,"config":`+invalid+`}}`))
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}))
		defer server.Close()
		wsCtx, stop := context.WithCancel(ctx)
		ws := panel.NewWSClient("ws"+strings.TrimPrefix(server.URL, "http"), "fixture", 7, panel.WSClientConfig{MachineID: pc.MachineID}, func(e panel.WSEvent) { events <- e }, nil, nil)
		done := make(chan struct{})
		go func() { defer close(done); ws.Run(wsCtx) }()
		defer func() { stop(); <-done }()
		select {
		case e := <-events:
			if e.NodeID != 7 || e.Type != panel.WSEventConfigPending {
				t.Fatalf("unexpected wire event %+v", e)
			}
			event, err := controlplane.TranslateWSEvent(e, s.cfg.Kernel)
			if err != nil {
				t.Fatal(err)
			}
			s.handleWSEvent(ctx, event)
			s.applyUserDelta(ctx, "add", []model.UserSpec{b})
			check(b, false)
		case <-time.After(3 * time.Second):
			t.Fatal("missing WS config rejection")
		}
	}
	pull := func() {
		s.pullViaAPIAsync(ctx)
		select {
		case r := <-s.pullResults:
			s.applyPullResult(ctx, r)
		case <-time.After(3 * time.Second):
			t.Fatal("missing poll result")
		}
	}
	pull()
	if !s.configPending || len(s.lastUsers) != 1 || s.lastUsers[0].SpeedLimit != 1 || s.lastUsers[0].DeviceLimit != 1 {
		t.Fatalf("restrictions after undecodable configuration: pending=%v users=%+v", s.configPending, s.lastUsers)
	}
	check(old, false)
	check(b, false)
	check(a, true)
	if snap, err := s.source.Poll(ctx); !errors.Is(err, panel.ErrIncompatibleConfig) || len(snap.Users) != 2 || snap.CommitUsers != nil || snap.CommitConfig != nil {
		t.Fatal("ETag accepted for rejected pair")
	}
	phase.Store(2)
	// The failed poll skips one scheduled retry.
	s.pullViaAPIAsync(ctx)
	pull()
	if s.configPending {
		t.Fatal("paired recovery still pending")
	}
	check(b, true)
	if snap, err := s.source.Poll(ctx); err != nil || snap.Config != nil || snap.Users != nil {
		t.Fatal("healthy pair failed to resume conditional requests")
	}
	if !s.applyUserUpdate(ctx, []model.UserSpec{a}, computeUserHash([]model.UserSpec{a})) {
		t.Fatal("reset fixture")
	}
}
