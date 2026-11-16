package machine

import (
	"context"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/panel"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWSConfigUsesResolvedNodeKernel(t *testing.T) {
	o := New(&config.Config{Kernel: config.KernelConfig{Type: "singbox"}, Machine: &config.MachineConfig{MachineID: 1}})
	mb := controlplane.NewNodeMailbox()
	mb.MarkReady()
	o.mailboxes[2] = mb
	o.kernels[2] = config.KernelConfig{Type: "xray"}
	o.onWSEvent(panel.WSEvent{Type: panel.WSEventSyncConfig, NodeID: 2, Config: &panel.NodeConfig{Protocol: "vless", Network: "xhttp", ServerPort: 20444}})
	state := mb.DrainIfReady()
	if !state.HasConfig || state.Config.Network != "xhttp" {
		t.Fatal("XHTTP config was not delivered to the Xray node")
	}
	o.unregisterNode(2)
	if _, ok := o.kernels[2]; ok {
		t.Fatal("removed node retained its kernel selection")
	}
}

func TestDiscoveryRetriesExitedNodesWithoutReplacingHealthyNodes(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	cfg := &config.Config{Panel: config.PanelConfig{URL: server.URL}, Kernel: config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir()}, Machine: &config.MachineConfig{MachineID: 1, Token: "test"}}
	o := New(cfg)
	done := make(chan struct{})
	close(done)
	canceled := false
	old := &nodeHandle{done: done, cancel: func() { canceled = true }}
	o.nodes[7] = old
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.startNode(ctx, panel.MachineNode{ID: 7, Type: "vless"})
	o.mu.Lock()
	fresh := o.nodes[7]
	o.mu.Unlock()
	if fresh == old || !canceled {
		t.Fatal("exited node was not retried")
	}
	select {
	case <-fresh.done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled retry did not exit")
	}
	live := &nodeHandle{done: make(chan struct{}), cancel: func() {}}
	o.nodes[8] = live
	before := calls.Load()
	o.startNode(ctx, panel.MachineNode{ID: 8, Type: "vless"})
	if o.nodes[8] != live || calls.Load() != before {
		t.Fatal("healthy node was restarted")
	}
}

func TestStopKeepsGenerationReservedUntilCleanupFinishes(t *testing.T) {
	o := New(&config.Config{Kernel: config.KernelConfig{Type: "singbox"}, Machine: &config.MachineConfig{MachineID: 1}})
	done := make(chan struct{})
	entered := make(chan struct{})
	release := make(chan struct{})
	h := &nodeHandle{done: done, cancel: func() { close(entered); <-release; close(done) }}
	o.nodes[9] = h
	stopped := make(chan struct{})
	go func() { o.stopNode(9); close(stopped) }()
	<-entered
	o.startNode(context.Background(), panel.MachineNode{ID: 9, Type: "vless"})
	o.mu.Lock()
	kept := o.nodes[9] == h
	o.mu.Unlock()
	if !kept {
		t.Fatal("discovery replaced a node before cleanup finished")
	}
	close(release)
	<-stopped
	o.mu.Lock()
	_, exists := o.nodes[9]
	o.mu.Unlock()
	if exists {
		t.Fatal("stopped generation retained")
	}
}
