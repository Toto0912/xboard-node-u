package model

import (
	"encoding/json"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestStandaloneSingleFallbackMatchesPanel(t *testing.T) {
	for _, kind := range []string{"xray", " Xray ", "singbox", "sing-box"} {
		for _, protocol := range []string{"vless", "trojan"} {
			for _, tls := range []int{1, 2} {
				for _, target := range []string{" EXAMPLE.test:08443 ", "[::1]:8443", "", "invalid", "host:0", "https://site.test:443", "8080"} {
					local := config.StandaloneNodeConfig{Protocol: protocol, ServerPort: 18443, Network: "raw", TLS: tls, XrayFallback: target,
						Flow: "xtls-rprx-vision", TLSSettings: map[string]any{"dest": "original.test:443", "server_name": "original.test", "private_key": "test-key", "short_id": "0123"}}
					cfg := &config.Config{Kernel: config.KernelConfig{Type: kind}, Standalone: &config.StandaloneConfig{Enabled: true, Node: local}}
					got, err := NodeSpecFromStandaloneValidated(cfg)
					if err != nil {
						t.Fatal(err)
					}
					remote, err := NodeSpecFromPanelValidated(&panel.NodeConfig{Protocol: protocol, ServerPort: 18443, Network: "raw", TLS: tls, XrayFallback: panel.OptionalFallbackTarget(target)}, cfg.Kernel)
					if err != nil || got.XrayFallback != remote.XrayFallback {
						t.Fatalf("%s/%s/tls%d/%q: standalone %q differs from panel: %v", kind, protocol, tls, target, got.XrayFallback, err)
					}
					if got.TLSSettings["dest"] != "original.test:443" || got.TLSSettings["server_name"] != "original.test" || got.Flow != local.Flow || cfg.Standalone.Node.XrayFallback != target {
						t.Fatal("conversion mutated the base Reality configuration")
					}
					if kind == "singbox" || kind == "sing-box" {
						cfg.Standalone.Node.XrayFallback = "another.test:443"
						other, err := NodeSpecFromStandaloneValidated(cfg)
						if err != nil {
							t.Fatal(err)
						}
						a, _ := json.Marshal(got)
						b, _ := json.Marshal(other)
						if string(a) != string(b) {
							t.Fatal("ignored setting changed the sing-box reload hash")
						}
					}
				}
			}
		}
	}
}

func TestStandaloneSingleFallbackEligibilityAndDisabledMode(t *testing.T) {
	for _, node := range []config.StandaloneNodeConfig{
		{Protocol: "vmess", TLS: 1}, {Protocol: "vless", TLS: 0},
		{Protocol: "vless", TLS: 1, Network: "ws"}, {Protocol: "trojan", TLS: 2, Network: "grpc"},
		{Protocol: "vless", TLS: 1, Decryption: "mlkem-test"},
	} {
		node.ServerPort, node.XrayFallback = 18443, "127.0.0.1:8080"
		cfg := &config.Config{Kernel: config.KernelConfig{Type: "xray"}, Standalone: &config.StandaloneConfig{Enabled: true, Node: node}}
		got, err := NodeSpecFromStandaloneValidated(cfg)
		if err != nil || got.XrayFallback != "" {
			t.Fatalf("unsupported optional target was not ignored: %+v %v", node, err)
		}
	}
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "xray"}, Standalone: &config.StandaloneConfig{Node: config.StandaloneNodeConfig{Protocol: "vless", TLS: 2, XrayFallback: "localhost:8443"}}}
	if NodeSpecFromStandalone(cfg).XrayFallback != "" {
		t.Fatal("disabled standalone config enabled the fallback")
	}
}
