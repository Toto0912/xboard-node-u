package model

import (
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
)

func TestStandaloneRealityFallbackMetadataPreservesHandshake(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "xray"}, Standalone: &config.StandaloneConfig{Enabled: true,
		Node: config.StandaloneNodeConfig{Protocol: "vless", ServerPort: 18443, Network: "raw", TLS: 2, Flow: "xtls-rprx-vision",
			TLSSettings: map[string]any{"dest": "original.test:443", "server_name": "original.test", "private_key": "test-key", "short_id": "0123"},
			Fallbacks:   []config.XrayFallback{{Dest: "127.0.0.1:8080"}, {ALPN: "h2", Dest: "127.0.0.1:8081"}}}}}
	node, err := NodeSpecFromStandaloneValidated(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if node.Standalone == nil || len(node.Standalone.Fallbacks) != 2 || node.Standalone.ALPN != nil || node.XrayFallback != "" {
		t.Fatal("Reality did not use standalone native rules")
	}
	if node.TLSSettings["dest"] != "original.test:443" || node.TLSSettings["server_name"] != "original.test" || node.Flow != "xtls-rprx-vision" {
		t.Fatal("fallback changed Reality handshake settings")
	}
	node.Standalone.Fallbacks[0].Dest = "127.0.0.1:9090"
	if cfg.Standalone.Node.Fallbacks[0].Dest != "127.0.0.1:8080" {
		t.Fatal("native fallback slice aliases input config")
	}
}
