package model

import (
	"encoding/json"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestPanelFallbackNormalizationAndKernelIsolation(t *testing.T) {
	for _, protocol := range []string{"vless", "trojan"} {
		for _, security := range []int{1, 2} {
			for _, kind := range []string{"xray", " Xray ", "singbox", "sing-box"} {
				nc := &panel.NodeConfig{Protocol: protocol, TLS: security, Network: "raw", ServerPort: 18443, XrayFallback: " EXAMPLE.test:08443 "}
				got, err := NodeSpecFromPanelValidated(nc, config.KernelConfig{Type: kind})
				if err != nil {
					t.Fatal(err)
				}
				want := ""
				if kind == "xray" || kind == " Xray " {
					want = "example.test:8443"
				}
				if got.XrayFallback != want || string(got.ToPanel().XrayFallback) != want {
					t.Fatalf("%s/%d/%s: %+v", protocol, security, kind, got)
				}
				if want == "" {
					nc.XrayFallback = "127.0.0.1:8080"
					other, err := NodeSpecFromPanelValidated(nc, config.KernelConfig{Type: kind})
					if err != nil {
						t.Fatal(err)
					}
					a, _ := json.Marshal(got)
					b, _ := json.Marshal(other)
					if string(a) != string(b) {
						t.Fatal("sing-box reload fingerprint changed for ignored setting")
					}
				}
			}
		}
	}
	for _, value := range []string{`null`, `false`, `123`, `[]`, `{"dest":"localhost:80"}`, `"bad"`, `"host:0"`} {
		var nc panel.NodeConfig
		if err := json.Unmarshal([]byte(`{"protocol":"vless","server_port":18443,"tls":1,"xray_fallback":`+value+`}`), &nc); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"xray", "singbox"} {
			got, err := NodeSpecFromPanelValidated(&nc, config.KernelConfig{Type: kind})
			if err != nil || got.XrayFallback != "" {
				t.Fatalf("malformed optional field must be ignored: %s %v", value, err)
			}
		}
	}
}

func TestPanelFallbackEligibility(t *testing.T) {
	for _, nc := range []*NodeSpec{
		{Protocol: "vmess", TLS: 1}, {Protocol: "vless", TLS: 0},
		{Protocol: "vless", TLS: 1, Network: "ws"}, {Protocol: "trojan", TLS: 2, Network: "grpc"},
		{Protocol: "vless", TLS: 1, Decryption: "mlkem-test"},
	} {
		nc.XrayFallback = "127.0.0.1:8080"
		if nc.PanelFallbackTarget() != "" {
			t.Fatalf("unsupported combination accepted: %+v", nc)
		}
	}
}
