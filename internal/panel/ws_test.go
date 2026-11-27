package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWSMachineRevokedRequiresExplicitDenial(t *testing.T) {
	for _, tt := range []struct {
		machineID int
		data      string
		want      bool
	}{
		{1, `{"code":403}`, true},
		{1, `{"code":401}`, true},
		{1, `{"code":503}`, false},
		{1, `{}`, false},
		{0, `{"code":403}`, false},
	} {
		got := false
		ws := NewWSClient("", "", 0, WSClientConfig{MachineID: tt.machineID}, func(event WSEvent) {
			got = event.Type == WSEventMachineRevoked
		}, nil, nil)
		ws.handleMessage(wsMessage{Event: "machine.revoked", Data: json.RawMessage(tt.data)})
		if got != tt.want {
			t.Fatalf("machine=%d data=%s delivered=%v; expected %v", tt.machineID, tt.data, got, tt.want)
		}
	}
}

// fakeWSServer simulates a minimal Workerman-style WS server for testing.
// It authenticates via token/node_id query params, sends auth.success, then
// delivers the provided events and handles pongs until the client disconnects.
func fakeWSServer(t *testing.T, events []wsMessage) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate query params
		q := r.URL.Query()
		if q.Get("token") == "" || (q.Get("node_id") == "" && q.Get("machine_id") == "") {
			http.Error(w, "missing auth params", http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		defer conn.Close()

		// Send auth.success
		if err := conn.WriteJSON(wsMessage{Event: "auth.success"}); err != nil {
			return
		}

		// Send test events
		for _, evt := range events {
			time.Sleep(50 * time.Millisecond)
			if err := conn.WriteJSON(evt); err != nil {
				return
			}
		}

		// Handle pongs/messages until connection closes
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Event == "pong" {
				// Client pong — acknowledged
			}
		}
	}))
}

func TestWSIncompatibleConfigurationIdentity(t *testing.T) {
	for _, machineID := range []int{0, 12} {
		samples := []struct {
			data string
			id   int
		}{
			{`{"node_id":"7","config":{"cert_config":{"http_port":{}},"protocol":"vless"}}`, 7},
			{`{"config":{"node_id":8}}`, 8},
			{`{"node_id":7,"config":[]}`, 7},
			{`{"node_id":7,"config":{"node_id":8}}`, 0},
			{`{"node_id":7.5,"config":{}}`, 0},
			{`{}`, 0},
		}
		var messages []wsMessage
		for _, sample := range samples {
			messages = append(messages, wsMessage{Event: WSEventSyncConfig, Data: json.RawMessage(sample.data)})
		}
		server := fakeWSServer(t, messages)
		events := make(chan WSEvent, len(samples))
		ctx, cancel := context.WithCancel(context.Background())
		client := NewWSClient("ws"+strings.TrimPrefix(server.URL, "http"), "fixture", 7, WSClientConfig{MachineID: machineID}, func(e WSEvent) { events <- e }, nil, nil)
		done := make(chan struct{})
		go func() { defer close(done); client.Run(ctx) }()
		func() {
			defer func() { cancel(); <-done; server.Close() }()
			for _, sample := range samples {
				select {
				case event := <-events:
					if event.Type != WSEventConfigPending || event.Config != nil || event.NodeID != sample.id {
						t.Fatalf("machine %d / %s: %+v", machineID, sample.data, event)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("undelivered config event")
				}
			}
		}()
	}
}

func TestWSClient_ConnectAndReceiveDataEvents(t *testing.T) {
	configPayload := syncConfigPayload{
		Config:    NodeConfig{Protocol: "vless", ServerPort: 443},
		Timestamp: 1234,
	}
	usersPayload := syncUsersPayload{
		Users:     []User{{ID: 1, UUID: "abc", SpeedLimit: 100, DeviceLimit: 3}},
		Timestamp: 1235,
	}

	configData, _ := json.Marshal(configPayload)
	usersData, _ := json.Marshal(usersPayload)

	events := []wsMessage{
		{Event: WSEventSyncConfig, Data: configData},
		{Event: WSEventSyncUsers, Data: usersData},
	}
	server := fakeWSServer(t, events)
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")

	var mu sync.Mutex
	var received []WSEvent

	ws := NewWSClient("ws://"+host, "test-token", 1, WSClientConfig{}, func(event WSEvent) {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ws.Run(ctx)

	// Wait for events
	deadline := time.After(1500 * time.Millisecond)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for events, got %d", n)
		case <-time.After(50 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 2 {
		t.Fatalf("expected 2 events, got %d", len(received))
	}

	// Verify config event
	if received[0].Type != WSEventSyncConfig {
		t.Errorf("event[0].Type = %q, want %q", received[0].Type, WSEventSyncConfig)
	}
	if received[0].Config == nil {
		t.Fatal("event[0].Config is nil")
	}
	if received[0].Config.Protocol != "vless" {
		t.Errorf("config.Protocol = %q, want %q", received[0].Config.Protocol, "vless")
	}
	if received[0].Config.ServerPort != 443 {
		t.Errorf("config.ServerPort = %d, want 443", received[0].Config.ServerPort)
	}

	// Verify users event
	if received[1].Type != WSEventSyncUsers {
		t.Errorf("event[1].Type = %q, want %q", received[1].Type, WSEventSyncUsers)
	}
	if len(received[1].Users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(received[1].Users))
	}
	if received[1].Users[0].UUID != "abc" {
		t.Errorf("user.UUID = %q, want %q", received[1].Users[0].UUID, "abc")
	}

	if !ws.IsConnected() {
		t.Error("expected IsConnected() = true while server is running")
	}
}

func TestWSClient_ReconnectOnDisconnect(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	var mu sync.Mutex
	connectCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		mu.Lock()
		connectCount++
		count := connectCount
		mu.Unlock()

		// Send auth.success
		conn.WriteJSON(wsMessage{Event: "auth.success"})

		// Close immediately on first connection to trigger reconnect
		if count == 1 {
			conn.Close()
			return
		}

		// Keep second connection alive
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	ws := NewWSClient("ws://"+host, "reconnect-token", 1, WSClientConfig{}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go ws.Run(ctx)

	// Wait for reconnection
	deadline := time.After(4 * time.Second)
	for {
		mu.Lock()
		n := connectCount
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("expected at least 2 connections, got %d", connectCount)
			mu.Unlock()
		case <-time.After(100 * time.Millisecond):
		}
	}

	if !ws.IsConnected() {
		t.Error("expected IsConnected() = true after reconnect")
	}
}

func TestWSClient_FallbackWhenNoServer(t *testing.T) {
	ws := NewWSClient("ws://127.0.0.1:19999", "fallback-token", 1, WSClientConfig{}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go ws.Run(ctx)

	time.Sleep(500 * time.Millisecond)
	if ws.IsConnected() {
		t.Error("expected IsConnected() = false when no server")
	}
}

func TestWSClient_UserDeltaEvent(t *testing.T) {
	deltaPayload := syncUserDeltaPayload{
		Action:    "add",
		Users:     []User{{ID: 42, UUID: "delta-uuid"}},
		Timestamp: 1236,
	}
	deltaData, _ := json.Marshal(deltaPayload)

	events := []wsMessage{
		{Event: WSEventSyncUserDelta, Data: deltaData},
	}
	server := fakeWSServer(t, events)
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")

	var mu sync.Mutex
	var received []WSEvent

	ws := NewWSClient("ws://"+host, "delta-token", 1, WSClientConfig{}, func(event WSEvent) {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ws.Run(ctx)

	deadline := time.After(1500 * time.Millisecond)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for delta event")
		case <-time.After(50 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if received[0].Type != WSEventSyncUserDelta {
		t.Errorf("event.Type = %q, want %q", received[0].Type, WSEventSyncUserDelta)
	}
	if received[0].DeltaAction != "add" {
		t.Errorf("event.DeltaAction = %q, want %q", received[0].DeltaAction, "add")
	}
	if len(received[0].DeltaUsers) != 1 || received[0].DeltaUsers[0].ID != 42 {
		t.Errorf("unexpected DeltaUsers: %+v", received[0].DeltaUsers)
	}
}

func TestWSClient_SyncUsersEmptyArrayPreservedAndMissingIgnored(t *testing.T) {
	var received []WSEvent
	ws := NewWSClient("ws://127.0.0.1:1", "tok", 1, WSClientConfig{}, func(event WSEvent) {
		received = append(received, event)
	}, nil, nil)

	// Missing "users" key must be ignored (must not wipe kernel users on malformed frame).
	ws.handleMessage(wsMessage{Event: WSEventSyncUsers, Data: json.RawMessage(`{"timestamp":100}`)})
	if len(received) != 0 {
		t.Fatalf("expected malformed sync.users without users key to be ignored, got %d events", len(received))
	}

	// Explicit empty "users": [] must be delivered with non-nil slice so 1->0 clears users.
	ws.handleMessage(wsMessage{Event: WSEventSyncUsers, Data: json.RawMessage(`{"users":[],"node_id":7}`)})
	if len(received) != 1 {
		t.Fatalf("expected 1 event for explicit empty users list, got %d", len(received))
	}
	if received[0].Users == nil || len(received[0].Users) != 0 {
		t.Fatalf("expected non-nil empty Users slice, got %#v", received[0].Users)
	}
	if received[0].NodeID != 7 {
		t.Fatalf("expected NodeID=7, got %d", received[0].NodeID)
	}
}

func TestWSIdleTimeoutAndActiveHeartbeat(t *testing.T) {
	for _, heartbeat := range []bool{false, true} {
		up := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			if err := c.WriteJSON(wsMessage{Event: "auth.success"}); err != nil {
				return
			}
			if !heartbeat {
				_, _, _ = c.ReadMessage()
				return
			}
			tick := time.NewTicker(15 * time.Millisecond)
			defer tick.Stop()
			for range tick.C {
				if err := c.WriteJSON(wsMessage{Event: "ping"}); err != nil {
					return
				}
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
		cl := NewWSClient("ws"+strings.TrimPrefix(srv.URL, "http"), "test", 1, WSClientConfig{}, nil, nil, nil)
		cl.readIdle = 100 * time.Millisecond
		err := cl.connect(ctx)
		expired := ctx.Err() != nil
		cancel()
		srv.Close()
		if heartbeat != expired || (!heartbeat && err == nil) {
			t.Fatalf("heartbeat=%v deadline=%v err=%v", heartbeat, expired, err)
		}
	}
}
