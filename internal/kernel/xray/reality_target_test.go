package xray

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/xtls/xray-core/infra/conf/serial"
)

func TestRealityTargetHonorsPanelPortAndExplicitDest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]any
		want     string
	}{
		{"default", map[string]any{"server_name": "site.test"}, "site.test:443"},
		{"string-port", map[string]any{"server_name": "site.test", "server_port": "8443"}, "site.test:8443"},
		{"numeric-port", map[string]any{"server_name": "site.test", "server_port": float64(9443)}, "site.test:9443"},
		{"ipv6", map[string]any{"server_name": "::1", "server_port": 8443}, "[::1]:8443"},
		{"explicit", map[string]any{"server_name": "site.test", "server_port": 8443, "dest": "127.0.0.1:9443"}, "127.0.0.1:9443"},
		{"panel-host-port", map[string]any{"server_name": "sni.test", "dest": "target.test", "server_port": float64(8443)}, "target.test:8443"},
		{"panel-ip-port", map[string]any{"server_name": "sni.test", "dest": "127.0.0.1", "server_port": "8443"}, "127.0.0.1:8443"},
		{"panel-default-port", map[string]any{"server_name": "sni.test", "dest": "target.test"}, "target.test:443"},
		{"empty-dest", map[string]any{"server_name": "site.test", "dest": "", "server_port": 8443}, "site.test:8443"},
		{"null-dest", map[string]any{"server_name": "site.test", "dest": nil, "server_port": 8443}, "site.test:8443"},
		{"trim-dest", map[string]any{"server_name": "site.test", "dest": " target.test ", "server_port": 8443}, "target.test:8443"},
		{"panel-ipv6", map[string]any{"server_name": "sni.test", "dest": "::1", "server_port": 8443}, "[::1]:8443"},
		{"panel-bracketed-ipv6", map[string]any{"server_name": "sni.test", "dest": "[::1]", "server_port": 8443}, "[::1]:8443"},
		{"explicit-ipv6", map[string]any{"server_name": "sni.test", "dest": "[::1]:9443", "server_port": 8443}, "[::1]:9443"},
		{"unix", map[string]any{"server_name": "sni.test", "dest": "/run/reality.sock"}, "/run/reality.sock"},
		{"abstract-unix", map[string]any{"server_name": "sni.test", "dest": "@reality"}, "@reality"},
		{"native-port", map[string]any{"server_name": "sni.test", "dest": "9443"}, "9443"},
		{"native-numeric-port", map[string]any{"server_name": "sni.test", "dest": 9443}, "9443"},
		{"invalid-url-not-rewritten", map[string]any{"server_name": "sni.test", "dest": "https://target.test", "server_port": 8443}, "https://target.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRealitySettings(&model.NodeSpec{TLSSettings: tc.settings})
			if got["dest"] != tc.want {
				t.Fatalf("got %v, want %s", got["dest"], tc.want)
			}
			if got["serverNames"].([]string)[0] != tc.settings["server_name"] {
				t.Fatal("target normalization changed SNI")
			}
		})
	}
}

func TestPanelRealitySeparateTargetPortParsesInXray(t *testing.T) {
	for _, protocol := range []string{"vless", "trojan"} {
		t.Run(protocol, func(t *testing.T) {
			// Both machine and single-node modes validate the panel NodeConfig
			// through this path before building the kernel configuration.
			payload := `{"protocol":"` + protocol + `","listen_ip":"127.0.0.1","server_port":18443,"network":"tcp","tls":2,"tls_settings":{"server_name":"sni.test","dest":"target.test","server_port":8443,"short_id":"0123456789abcdef"}}`
			var panelConfig panel.NodeConfig
			if err := json.Unmarshal([]byte(payload), &panelConfig); err != nil {
				t.Fatal(err)
			}
			panelConfig.TLSSettings["private_key"] = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
			node, err := model.NodeSpecFromPanelValidated(&panelConfig, testKernelCfg)
			if err != nil {
				t.Fatal(err)
			}
			cfg := buildConfig(testKernelCfg, node, testUsers, kernel.TLSCert{})
			settings := cfg["inbounds"].([]M)[0]["streamSettings"].(M)["realitySettings"].(M)
			if settings["dest"] != "target.test:8443" || settings["serverNames"].([]string)[0] != "sni.test" {
				t.Fatalf("unexpected Reality settings: %v", settings)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := serial.LoadJSONConfig(bytes.NewReader(raw)); err != nil {
				t.Fatalf("generated panel config rejected by Xray: %v", err)
			}
		})
	}
}
