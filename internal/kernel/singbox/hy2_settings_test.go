package singbox

import (
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"testing"
)

func TestHY2BandwidthPolicyAndMasquerade(t *testing.T) {
	for _, test := range []struct {
		up, down int
		auto     bool
	}{{0, 0, false}, {23, 47, false}, {0, 0, true}, {23, 47, true}, {0, 47, true}} {
		n := &model.NodeSpec{Protocol: "hysteria", Version: 2, UpMbps: test.up, DownMbps: test.down}
		if test.auto {
			n.HY2Settings = &config.HY2Settings{BandwidthMode: "auto", Kernel: "singbox", Masquerade: map[string]any{"type": "string", "content": "hello", "status_code": 200}}
		}
		in := buildInbound(n, nil, kernel.TLSCert{})
		if test.up > 0 && in["up_mbps"] != test.up {
			t.Fatal("server upload mapping missing")
		}
		if test.down > 0 && in["down_mbps"] != test.down {
			t.Fatal("server download mapping missing")
		}
		if test.auto {
			if in["ignore_client_bandwidth"] != (test.up == 0 && test.down == 0) {
				t.Fatal("incorrect auto policy")
			}
			if in["masquerade"].(map[string]any)["content"] != "hello" {
				t.Fatal("masquerade lost")
			}
		} else if _, exists := in["ignore_client_bandwidth"]; exists {
			t.Fatal("legacy policy changed")
		}
		for _, key := range []string{"idle_timeout", "keep_alive_period", "stream_receive_window", "bbr_profile"} {
			if _, ok := in[key]; ok {
				t.Fatalf("QUIC default overridden: %s", key)
			}
		}
	}
}
