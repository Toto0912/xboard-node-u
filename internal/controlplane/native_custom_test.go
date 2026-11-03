package controlplane

import (
	"context"
	"encoding/json"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/panel"
	"testing"
)

func TestNativeCustomConfigRESTPollAndWebSocket(t *testing.T) {
	body := `{"protocol":"vless","server_port":18443,"custom_outbounds":[{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":1080,"detour":"direct","udp_over_tcp":{}}],"custom_routes":[{"action":"route","outbound":"proxy"}]}`
	server := newPanelTestServer(body)
	defer server.Close()
	cp := NewPanelControlPlane(config.PanelConfig{URL: server.URL, Token: "token", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: "singbox"})
	initial, err := cp.Initial(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	poll, err := cp.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*model.NodeSpec{initial.Config, poll.Config} {
		if n.CustomOutbounds[0].Type != "socks" || n.CustomOutbounds[0].Extra["detour"] != "direct" {
			t.Fatalf("lost native fields: %#v", n)
		}
	}
	var nc panel.NodeConfig
	if err := json.Unmarshal([]byte(body), &nc); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{0, 9} {
		nc.NodeID = id
		_, err := TranslateWSEvent(panel.WSEvent{Type: panel.WSEventSyncConfig, Config: &nc}, config.KernelConfig{Type: "singbox"})
		if err != nil {
			t.Fatal(err)
		}
	}
}
