package machine

import (
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/panel"
	"testing"
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
