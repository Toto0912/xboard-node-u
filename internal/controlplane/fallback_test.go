package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestFallbackRESTAndWSForSingleAndMachine(t *testing.T) {
	for _, kind := range []string{"xray", "singbox"} {
		want := ""
		if kind == "xray" {
			want = "127.0.0.1:8443"
		}
		body := `{"protocol":"vless","server_port":18443,"tls":2,"network":"tcp","xray_fallback":"127.0.0.1:8443"}`
		server := newPanelTestServer(body)
		single := NewPanelControlPlane(config.PanelConfig{URL: server.URL, Token: "test", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: kind})
		snapshot, err := single.Poll(context.Background())
		server.Close()
		if err != nil || snapshot.Config.XrayFallback != want {
			t.Fatalf("single/%s: %+v %v", kind, snapshot, err)
		}
		machineServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v2/server/config" {
				_, _ = w.Write([]byte(body))
			} else {
				_, _ = w.Write([]byte(`{"users":[]}`))
			}
		}))
		machine := NewMachinePanelControlPlane(panel.NewClient(config.PanelConfig{URL: machineServer.URL, MachineID: 1, Token: "test", NodeID: 1}), config.KernelConfig{Type: kind}, nil, nil)
		snapshot, err = machine.Poll(context.Background())
		machineServer.Close()
		if err != nil || snapshot.Config.XrayFallback != want {
			t.Fatalf("machine/%s: %+v %v", kind, snapshot, err)
		}
		event, err := TranslateWSEvent(panel.WSEvent{Type: panel.WSEventSyncConfig, Config: &panel.NodeConfig{Protocol: "vless", TLS: 2, ServerPort: 18443, XrayFallback: "127.0.0.1:8443"}}, config.KernelConfig{Type: kind})
		if err != nil || event.Config.XrayFallback != want {
			t.Fatalf("ws/%s: %+v %v", kind, event, err)
		}
	}
}
