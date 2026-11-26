package singbox

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	J "github.com/sagernet/sing/common/json"
)

func checkHotUsers[T any](t *testing.T, raw []byte, cold []T) {
	t.Helper()
	err := updateEncodedUsers(raw, func(hot []T) error {
		if !reflect.DeepEqual(cold, hot) {
			t.Fatalf("cold users %v, hot users %v", cold, hot)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHotUserOptionsMatchColdStart(t *testing.T) {
	for _, node := range []model.NodeSpec{
		{Protocol: "vmess"}, {Protocol: "vless", Flow: "xtls-rprx-vision"}, {Protocol: "trojan"},
		{Protocol: "shadowsocks", Cipher: "aes-128-gcm"}, {Protocol: "shadowsocks", Cipher: "2022-blake3-aes-128-gcm"},
		{Protocol: "shadowsocks", Cipher: "2022-blake3-aes-256-gcm"}, {Protocol: "tuic"}, {Protocol: "hysteria", Version: 1},
		{Protocol: "hysteria", Version: 2}, {Protocol: "hysteria2"}, {Protocol: "anytls"}, {Protocol: "socks"}, {Protocol: "http"},
		{Protocol: "naive"}, {Protocol: "mieru", Transport: "TCP"},
	} {
		t.Run(node.Protocol+node.Cipher, func(t *testing.T) {
			node.ServerPort = 1443
			ctx := include.Context(context.Background())
			for _, users := range [][]model.UserSpec{nil, testUsers[:2]} {
				cfg := config.KernelConfig{Type: "singbox", LogLevel: "error"}
				whole, err := json.Marshal(buildConfig(cfg, &node, users, kernel.TLSCert{}))
				if err != nil {
					t.Fatal(err)
				}
				options, err := J.UnmarshalExtendedContext[option.Options](ctx, whole)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(buildInbound(&node, users, kernel.TLSCert{})["users"])
				if err != nil {
					t.Fatal(err)
				}
				switch opts := options.Inbounds[0].Options.(type) {
				case *option.VMessInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.VLESSInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.TrojanInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.ShadowsocksInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.HysteriaInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.Hysteria2InboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.TUICInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.AnyTLSInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.SocksInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.HTTPMixedInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.NaiveInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				case *option.MieruInboundOptions:
					checkHotUsers(t, raw, opts.Users)
				default:
					t.Fatalf("unsupported assertion %T", opts)
				}
			}
		})
	}
}
