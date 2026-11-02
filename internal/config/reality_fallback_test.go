package config

import (
	"strings"
	"testing"
)

func TestLoadStandaloneRealityNativeFallbacks(t *testing.T) {
	for _, prefix := range []string{"", "instances:\n  - id: local\n"} {
		content := `kernel: {type: xray}
standalone:
  enabled: true
  node:
    protocol: vless
    server_port: 18443
    network: raw
    tls: 2
    tls_settings: {server_name: site.test, dest: "original.test:443"}
    fallbacks:
      - {dest: "127.0.0.1:8080"}
      - {name: site.test, alpn: h2, dest: "[::1]:8081", xver: 2}
      - {path: /special, dest: 8082}
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
		if err != nil || len(instances) != 1 {
			t.Fatalf("normalize instances: %v", err)
		}
		node := instances[0].Standalone.Node
		if len(node.Fallbacks) != 3 || node.TLSSettings["dest"] != "original.test:443" {
			t.Fatal("native list lost or replaced Reality dest")
		}
	}
}

func TestRealityFallbackValidationUsesNegotiatedALPN(t *testing.T) {
	node := fallbackNode()
	node.TLS = 2
	node.TLSSettings = map[string]any{"alpn": "not-a-TLS-list"}
	node.Fallbacks = append(node.Fallbacks, XrayFallback{ALPN: "h2", Dest: "127.0.0.1:8081"})
	if err := node.ValidateFallbacks("xray"); err != nil {
		t.Fatalf("Reality must not validate ALPN against ordinary TLS settings: %v", err)
	}
	node.Decryption = "mlkem768x25519plus.native.test"
	if err := node.ValidateFallbacks("xray"); err == nil {
		t.Fatal("native Reality fallbacks cannot bypass VLESS Encryption restriction")
	}
}
