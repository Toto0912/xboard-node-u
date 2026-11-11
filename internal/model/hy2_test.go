package model

import (
	"encoding/json"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
	"testing"
)

func TestHY2RoundTripAndStandalone(t *testing.T) {
	original := &panel.NodeConfig{Protocol: "hysteria", Version: 2, UpMbps: 47, DownMbps: 23, HY2Settings: &config.HY2Settings{Kernel: "singbox", BandwidthMode: "auto", Masquerade: map[string]any{"type": "string", "content": "ok", "headers": map[string]any{"X-Test": []any{"one", "two"}}}}}
	spec := NodeSpecFromPanel(original)
	wire := spec.ToPanel()
	if wire.UpMbps != 47 || wire.DownMbps != 23 || wire.HY2Settings.Masquerade["content"] != "ok" {
		t.Fatal("server bandwidth swapped again or extension lost")
	}
	wire.HY2Settings.Masquerade["headers"].(map[string]any)["X-Test"].([]any)[0] = "changed"
	if spec.HY2Settings.Masquerade["headers"].(map[string]any)["X-Test"].([]any)[0] != "one" {
		t.Fatal("round trip shares nested header slices")
	}
	before, _ := json.Marshal(spec)
	spec.HY2Settings.Masquerade["content"] = "new"
	after, _ := json.Marshal(spec)
	if string(before) == string(after) {
		t.Fatal("config change absent from hash input")
	}
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}, Standalone: &config.StandaloneConfig{Enabled: true, Node: config.StandaloneNodeConfig{Protocol: "hysteria", Version: 2, UpMbps: 47, DownMbps: 23, HY2Settings: original.HY2Settings}}}
	standalone := NodeSpecFromStandalone(cfg)
	if standalone.UpMbps != 47 || standalone.DownMbps != 23 || standalone.HY2Settings == original.HY2Settings {
		t.Fatal("standalone changed server perspective or shares extension")
	}
	for _, obfs := range []struct {
		kind, password string
		valid          bool
	}{{"salamander", "test", true}, {"gecko", "test", true}, {"unknown", "test", false}, {"salamander", "", false}} {
		standalone.Obfs, standalone.ObfsPassword = obfs.kind, obfs.password
		if (ValidateNodeSpec(standalone, cfg.Kernel) == nil) != obfs.valid {
			t.Fatalf("unexpected obfs validation: %s", obfs.kind)
		}
	}
}
