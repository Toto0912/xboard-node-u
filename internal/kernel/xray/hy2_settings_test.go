package xray

import (
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"testing"
)

func TestHY2BandwidthPolicyMasqueradeAndObfs(t *testing.T) {
	for _, up := range []int{0, 23} {
		n := &model.NodeSpec{Protocol: "hysteria", Version: 2, UpMbps: up, DownMbps: up * 2, Obfs: "salamander", ObfsPassword: "example-password", HY2Settings: &config.HY2Settings{BandwidthMode: "auto", Kernel: "xray", Masquerade: map[string]any{"type": "string", "content": "hello", "statusCode": 200}}}
		in := buildInbound(n, nil, kernel.TLSCert{})
		ss := in["streamSettings"].(M)
		mask := ss["finalMask"].(M)
		params := mask["quicParams"].(M)
		want := "bbr"
		if up > 0 {
			want = "brutal"
		}
		if params["congestion"] != want {
			t.Fatalf("wrong policy %v", params)
		}
		if up > 0 && (params["brutalUp"] != "23000000" || params["brutalDown"] != "46000000") {
			t.Fatal("server directions lost")
		}
		if len(mask["udp"].([]M)) != 1 {
			t.Fatal("obfs dropped")
		}
		if ss["hysteriaSettings"].(M)["masquerade"].(map[string]any)["content"] != "hello" {
			t.Fatal("masquerade dropped")
		}
		if _, ok := params["maxIdleTimeout"]; ok {
			t.Fatal("QUIC default overridden")
		}
	}
}
