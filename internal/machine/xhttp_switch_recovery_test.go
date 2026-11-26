package machine

import (
	"encoding/json"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/panel"
)

func switchListenerOpen(addr string) bool {
	c, err := net.DialTimeout("tcp4", addr, 100*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestKernelSwitchPollStillRevokesUsersIfXHTTPIsInvalid(t *testing.T) {
	f := newSwitchBoundaryNode(t, false, 1)
	f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		config := f.tcp
		config.Network = "xhttp"
		config.NetworkSettings = map[string]any{"mode": "unsupported-mode"}
		_ = json.NewEncoder(w).Encode(config)
	}))
	f.userReply.Store(`{"users":[]}`)
	// A normal periodic pull must still apply the empty user list. No WS
	// revocation or replacement kernel is available to correct the old state.
	switchTestWait(t, "old listener to close after REST revocation", func() bool {
		return !switchListenerOpen(f.addr)
	})
	f.rediscover(f.ctx)
	if f.nodes[7] != f.first {
		t.Fatal("revocation was followed by a replacement with an invalid XHTTP config")
	}
}

func TestKernelSwitchBindFailureRecoversAndRetries(t *testing.T) {
	f := newSwitchBoundaryNode(t, false, 3600)
	free := switchTestListen(t)
	dest := free.Addr().String()
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	var requests atomic.Int32
	conflict := make(chan net.Listener, 1)
	f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The port is free when the machine validates the candidate. Another
		// process acquires it only when the replacement is bootstrapping.
		if requests.Add(1) == 2 {
			l, err := net.Listen("tcp4", dest)
			if err != nil {
				t.Errorf("occupying candidate port: %v", err)
				http.Error(w, "fixture", http.StatusInternalServerError)
				return
			}
			t.Cleanup(func() { l.Close() })
			conflict <- l
		}
		cfg := f.tcp
		cfg.Network, cfg.ServerPort = "xhttp", port
		_ = json.NewEncoder(w).Encode(cfg)
	}))
	f.pushConfig("xhttp")
	f.rediscover(f.ctx)
	candidate := f.nodes[7]
	if candidate == nil || candidate == f.first {
		t.Fatal("Xray candidate did not start")
	}
	select {
	case <-candidate.done:
	case <-time.After(12 * time.Second):
		t.Fatal("Xray candidate did not exit after failing to bind")
	}
	f.rediscover(f.ctx)
	recovery := f.nodes[7]
	switchTestWait(t, "recovered sing-box listener", func() bool { return switchListenerOpen(f.addr) })
	f.waitReady(t, recovery)
	if recovery == candidate || recovery == f.first {
		t.Fatal("old descriptor was not restored by a fresh service")
	}

	// The next machine discovery interval elapses after the conflicting
	// process releases the port. The panel now authorizes nobody.
	select {
	case l := <-conflict:
		l.Close()
	default:
		t.Fatal("fixture did not occupy the port")
	}
	f.userReply.Store(`{"users":[]}`)
	f.mu.Lock()
	recovery.nextCheck = time.Time{}
	f.mu.Unlock()
	f.rediscover(f.ctx)
	if f.nodes[7] == recovery {
		t.Fatal("machine did not retry the pending switch")
	}
	switchTestWait(t, "new Xray child to register its post-fetch mailbox", func() bool {
		f.eventsMu.RLock()
		defer f.eventsMu.RUnlock()
		return f.kernels[7].Type == "xray" && f.statuses[7] != nil
	})
	if switchListenerOpen(f.addr) || switchListenerOpen(dest) {
		t.Fatal("a listener was started with an empty authoritative user list")
	}
	// An explicit new grant starts the same child on the recovered port.
	f.onWSEvent(panel.WSEvent{NodeID: 7, Type: panel.WSEventSyncUsers,
		Users: []panel.User{{ID: 2, UUID: "22222222-2222-2222-2222-222222222222"}}})
	switchTestWait(t, "new grant to open recovered XHTTP port", func() bool { return switchListenerOpen(dest) })
	if switchListenerOpen(f.addr) {
		t.Fatal("previous TCP port reopened after the switch")
	}
}

func TestKernelSwitchKeepsOriginalWhenNewPortAlreadyInUse(t *testing.T) {
	f := newSwitchBoundaryNode(t, false, 3600)
	busy := switchTestListen(t)
	f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cfg := f.tcp
		cfg.Network, cfg.ServerPort = "xhttp", busy.Addr().(*net.TCPAddr).Port
		_ = json.NewEncoder(w).Encode(cfg)
	}))
	f.pushConfig("xhttp")
	f.rediscover(f.ctx)
	f.requireOriginalListener(t)
}
