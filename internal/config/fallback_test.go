package config

import (
	"os"
	"strings"
	"testing"
)

func fallbackNode() StandaloneNodeConfig {
	return StandaloneNodeConfig{Protocol: "vless", Network: "tcp", TLS: 1,
		Fallbacks: []XrayFallback{{Dest: "127.0.0.1:8080"}}}
}

func TestStandaloneFallbackValidation(t *testing.T) {
	for _, dest := range []any{8080, "8080", "127.0.0.1:8080", "localhost:8080", "[::1]:8080", "/run/site.sock", "@site", "@@site"} {
		node := fallbackNode()
		node.Fallbacks[0].Dest = dest
		if err := node.ValidateFallbacks("xray"); err != nil {
			t.Errorf("valid destination %v: %v", dest, err)
		}
	}
	for _, dest := range []any{nil, true, 80.5, 0, -1, 65536, "", " ", "abc", "host:0", ":8080", "::1:8080", "host:65536", "@", "/", "127.0.0.1:8080\n"} {
		node := fallbackNode()
		node.Fallbacks[0].Dest = dest
		if err := node.ValidateFallbacks("xray"); err == nil {
			t.Errorf("invalid destination %#v accepted", dest)
		}
	}
	for _, tc := range []struct {
		name   string
		edit   func(*StandaloneNodeConfig)
		kernel string
	}{
		{"singbox", func(*StandaloneNodeConfig) {}, "singbox"},
		{"shadowsocks", func(n *StandaloneNodeConfig) { n.Protocol = "shadowsocks" }, "xray"},
		{"ws", func(n *StandaloneNodeConfig) { n.Network = "ws" }, "xray"},
		{"no-tls", func(n *StandaloneNodeConfig) { n.TLS = 0 }, "xray"},
		{"reality", func(n *StandaloneNodeConfig) { n.TLS = 2 }, "xray"},
		{"decryption", func(n *StandaloneNodeConfig) { n.Decryption = "mlkem768x25519plus.native.test" }, "xray"},
		{"negative-xver", func(n *StandaloneNodeConfig) { n.Fallbacks[0].Xver = -1 }, "xray"},
		{"large-xver", func(n *StandaloneNodeConfig) { n.Fallbacks[0].Xver = 3 }, "xray"},
		{"path", func(n *StandaloneNodeConfig) { n.Fallbacks[0].Path = "missing-slash" }, "xray"},
		{"alpn-mismatch", func(n *StandaloneNodeConfig) { n.Fallbacks[0].ALPN = "h2" }, "xray"},
		{"h2-path", func(n *StandaloneNodeConfig) {
			n.TLSSettings = map[string]any{"alpn": []string{"h2", "http/1.1"}}
			n.Fallbacks[0].ALPN, n.Fallbacks[0].Path = "h2", "/ws"
		}, "xray"},
		{"alpn-scalar", func(n *StandaloneNodeConfig) { n.TLSSettings = map[string]any{"alpn": "http/1.1"} }, "xray"},
		{"alpn-empty", func(n *StandaloneNodeConfig) { n.TLSSettings = map[string]any{"alpn": []any{}} }, "xray"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := fallbackNode()
			tc.edit(&node)
			if err := node.ValidateFallbacks(tc.kernel); err == nil {
				t.Fatal("invalid fallback accepted")
			}
		})
	}
}

func TestLoadRootFallbacksAreInstanceScoped(t *testing.T) {
	path := writeTemp(t, `
instances:
  - id: panel-node
    kernel: {type: xray}
    panel: {url: "https://panel.example.com", token: "test", node_id: 1}
  - id: panel-machine
    kernel: {type: singbox}
    panel: {url: "https://panel.example.com"}
    machine: {machine_id: 1, token: "test"}
  - id: local-fallback
    kernel: {type: xray}
    standalone:
      enabled: true
      node:
        protocol: trojan
        server_port: 8443
        tls: 1
        tls_settings: {alpn: [h2, http/1.1]}
        fallbacks:
          - {dest: 8080}
          - {name: "site.example.com", alpn: h2, dest: "[::1]:8081", xver: 2}
      users: [{id: 1, uuid: "local-test-password"}]
`)
	root, err := LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Instances) != 3 || root.Instances[0].IsStandalone() || !root.Instances[1].IsMachineMode() {
		t.Fatal("panel modes changed in mixed-instance config")
	}
	local := root.Instances[2]
	if len(local.Standalone.Node.Fallbacks) != 2 || local.Standalone.Node.Fallbacks[0].Dest != 8080 {
		t.Fatal("fallback YAML values did not survive loading")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.ReplaceAll(string(content), "kernel: {type: xray}", "kernel: {type: singbox}")
	if _, err := LoadRoot(writeTemp(t, bad)); err == nil || !strings.Contains(err.Error(), "fallbacks requires kernel.type: xray") {
		t.Fatalf("expected actionable standalone error, got %v", err)
	}
}

func TestDisabledStandaloneFallbacksDoNotChangePanelModes(t *testing.T) {
	for _, machine := range []string{"", "machine: {machine_id: 1, token: test}\n"} {
		cfg, err := Load(writeTemp(t, `
kernel: {type: singbox}
panel: {url: "https://panel.example.com", token: test, node_id: 1}
standalone:
  enabled: false
  node:
    fallbacks: [{dest: null, xver: -1}]
`+machine))
		if err != nil || cfg.IsStandalone() {
			t.Fatalf("disabled standalone config changed panel mode: %v", err)
		}
		if cfg.IsMachineMode() != (machine != "") {
			t.Fatal("machine selection changed")
		}
	}
}
