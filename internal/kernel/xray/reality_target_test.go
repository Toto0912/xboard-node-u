package xray

import (
	"testing"

	"github.com/cedar2025/xboard-node/internal/model"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRealitySettings(&model.NodeSpec{TLSSettings: tc.settings})
			if got["dest"] != tc.want {
				t.Fatalf("got %v, want %s", got["dest"], tc.want)
			}
		})
	}
}
