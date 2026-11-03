package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestNativeCustomConfigValidation(t *testing.T) {
	for _, tc := range []struct{ name, kernel, outbounds, routes, want string }{
		{"optional blank detour", "singbox", `[{"type":"socks","tag":"proxy","detour":"","server":"127.0.0.1","server_port":1080}]`, `[{"action":"route","outbound":"proxy"}]`, ""},
		{"singbox native", "singbox", `[{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":1080,"version":"5","detour":"direct"}]`, `[{"action":"route","outbound":"proxy"}]`, ""},
		{"legacy singbox", "singbox", `[{"protocol":"socks","tag":"proxy","settings":{"server":"127.0.0.1","server_port":1080}}]`, `[{"action":"route","outbound":"proxy"}]`, ""},
		{"xray native extras", "xray", `[{"protocol":"socks","tag":"proxy","settings":{"servers":[{"address":"127.0.0.1","port":1080}]},"streamSettings":{"sockopt":{"mark":123}},"sendThrough":"0.0.0.0"}]`, `[{"localPort":"443","outboundTag":"proxy"}]`, ""},
		{"empty direct settings", "xray", `[{"protocol":"freedom","tag":"proxy","settings":{}}]`, `[{"network":"tcp,udp","outboundTag":"proxy"}]`, ""},
		{"wrong core", "xray", `[{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":1080}]`, `[]`, "sing-box native"},
		{"unknown detour", "singbox", `[{"type":"socks","tag":"proxy","detour":"missing"}]`, `[]`, "unknown outbound"},
		{"case sensitive", "singbox", `[{"type":"direct","tag":"Proxy"}]`, `[{"outbound":"proxy"}]`, "unknown outbound"},
		{"cycle", "singbox", `[{"type":"socks","tag":"a","detour":"b"},{"type":"socks","tag":"b","detour":"a"}]`, `[]`, "dependency cycle"},
		{"selector reference", "singbox", `[{"type":"selector","tag":"select","outbounds":["missing"]}]`, `[]`, "unknown outbound"},
		{"xray chain", "xray", `[{"protocol":"socks","tag":"proxy","settings":{"address":"127.0.0.1"},"proxySettings":{"tag":"direct"}}]`, `[]`, ""},
		{"xray unknown chain", "xray", `[{"protocol":"socks","tag":"proxy","settings":{"address":"127.0.0.1"},"streamSettings":{"sockopt":{"dialerProxy":"missing"}}}]`, `[]`, "unknown outbound"},
		{"mixed route", "xray", `[]`, `[{"action":"route","outbound":"direct"}]`, "not an Xray"},
		{"wrong route", "singbox", `[]`, `[{"outboundTag":"direct","localPort":"443"}]`, "not a sing-box"},
		{"logical route", "singbox", `[]`, `[{"type":"logical","mode":"or","rules":[{"domain_suffix":["example.com"]},{"port":443}],"action":"route","outbound":"direct"}]`, ""},
		{"unknown route", "xray", `[]`, `[{"outboundTag":"missing","localPort":"443"}]`, "unknown outbound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var nc panel.NodeConfig
			if err := json.Unmarshal([]byte(`{"protocol":"vless","server_port":18443,"network":"tcp","custom_outbounds":`+tc.outbounds+`,"custom_routes":`+tc.routes+`}`), &nc); err != nil {
				t.Fatal(err)
			}
			_, err := NodeSpecFromPanelValidated(&nc, config.KernelConfig{Type: tc.kernel})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNativeOutboundRoundTripAndHashFields(t *testing.T) {
	for _, raw := range []string{
		`{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":1080,"detour":"direct","udp_over_tcp":{},"future":{"flag":false}}`,
		`{"protocol":"freedom","tag":"proxy","settings":{},"streamSettings":{"sockopt":{"mark":123}},"mux":{"enabled":false}}`,
	} {
		var outbound panel.OutboundConfig
		if err := json.Unmarshal([]byte(raw), &outbound); err != nil {
			t.Fatal(err)
		}
		spec := NodeSpecFromPanel(&panel.NodeConfig{CustomOutbounds: []panel.OutboundConfig{outbound}})
		encoded, err := json.Marshal(spec.ToPanel().CustomOutbounds[0])
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		json.Unmarshal([]byte(raw), &want)
		json.Unmarshal(encoded, &got)
		w, _ := json.Marshal(want)
		g, _ := json.Marshal(got)
		if string(w) != string(g) {
			t.Fatalf("lost native fields: %s != %s", w, g)
		}
	}
}
