package config

import (
	"strings"
	"testing"
)

func TestLoadStandaloneSingleFallback(t *testing.T) {
	for _, prefix := range []string{"", "instances:\n  - id: local\n"} {
		for _, kind := range []string{"xray", "singbox"} {
			content := "kernel: {type: " + kind + `}
standalone:
  enabled: true
  node:
    protocol: vless
    server_port: 18443
    network: raw
    tls: 2
    xray_fallback: "127.0.0.1:8443"
    tls_settings: {server_name: site.test, dest: "original.test:443"}
  users: [{id: 1, uuid: "00000000-0000-4000-8000-000000000001"}]
`
			if prefix != "" {
				content = prefix + "    " + strings.ReplaceAll(strings.TrimSuffix(content, "\n"), "\n", "\n    ") + "\n"
			}
			root, err := LoadRoot(writeTemp(t, content))
			if err != nil {
				t.Fatal(err)
			}
			instances, err := root.NormalizeInstances()
			if err != nil {
				t.Fatal(err)
			}
			if len(instances) != 1 || instances[0].Standalone.Node.XrayFallback != "127.0.0.1:8443" {
				t.Fatal("single target lost during YAML loading")
			}
		}
	}
}

func TestStandaloneFallbackConflictAndRealityMigration(t *testing.T) {
	node := fallbackNode()
	node.XrayFallback = "localhost:8443"
	if err := node.ValidateFallbacks("xray"); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected actionable conflict: %v", err)
	}
	node.XrayFallback = "invalid"
	if err := node.ValidateFallbacks("xray"); err != nil {
		t.Fatalf("invalid optional target should not mask native rules: %v", err)
	}
	node.XrayFallback, node.TLS = "", 2
	if err := node.ValidateFallbacks("xray"); err == nil || !strings.Contains(err.Error(), "xray_fallback: host:port") {
		t.Fatalf("expected Reality migration hint: %v", err)
	}
}
