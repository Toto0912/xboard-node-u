package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestStandaloneFallbackMetadataIsolation(t *testing.T) {
	cfg := &config.Config{
		Kernel: config.KernelConfig{Type: "xray"},
		Standalone: &config.StandaloneConfig{Enabled: true,
			Node: config.StandaloneNodeConfig{Protocol: "vless", ServerPort: 443, TLS: 1,
				Fallbacks: []config.XrayFallback{{Dest: "127.0.0.1:8080"}}}},
	}
	node, err := NodeSpecFromStandaloneValidated(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if node.Standalone == nil || node.Standalone.Fallbacks[0].Dest != "127.0.0.1:8080" {
		t.Fatal("local fallback metadata missing")
	}
	node.Standalone.Fallbacks[0].Dest = "127.0.0.1:9090"
	if cfg.Standalone.Node.Fallbacks[0].Dest != "127.0.0.1:8080" {
		t.Fatal("instance metadata aliases original fallback slice")
	}

	// Both single-node and machine control planes use this panel conversion.
	remote := NodeSpecFromPanel(&panel.NodeConfig{Protocol: "vless", TLS: 1,
		TLSSettings: map[string]any{"fallbacks": []any{map[string]any{"dest": 8080}}}})
	if remote.Standalone != nil {
		t.Fatal("panel data acquired standalone privileges")
	}
	data, _ := json.Marshal(remote)
	if strings.Contains(string(data), `"Standalone"`) {
		t.Fatal("new empty field changes legacy panel config hash input")
	}

	cfg.Standalone.Enabled = false
	if NodeSpecFromStandalone(cfg).Standalone != nil {
		t.Fatal("disabled standalone config enabled fallbacks")
	}
	cfg.Standalone.Enabled = true
	cfg.Kernel.Type = "singbox"
	if _, err := NodeSpecFromStandaloneValidated(cfg); err == nil {
		t.Fatal("programmatic local control plane bypassed fallback validation")
	}
}
