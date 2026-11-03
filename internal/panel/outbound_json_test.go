package panel

import (
	"encoding/json"
	"testing"
)

func TestNativeOutboundRejectsAmbiguousAndMalformedObjects(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"type":""}`, `{"type":"socks","protocol":"socks"}`, `{"type":"socks","settings":{}}`, `{"type":"socks","proxy_tag":""}`, `{"type":42}`} {
		var out OutboundConfig
		if err := json.Unmarshal([]byte(raw), &out); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestNativeCustomWebSocketWeakDecodePreservesFields(t *testing.T) {
	var raw map[string]interface{}
	json.Unmarshal([]byte(`{"node_id":9,"config":{"protocol":"vless","custom_outbounds":[{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":1080,"udp_over_tcp":{},"detour":"direct"}]}}`), &raw)
	var payload syncConfigPayload
	if err := decodeWeakRaw(raw, &payload); err != nil {
		t.Fatal(err)
	}
	outbound := payload.Config.CustomOutbounds[0]
	if outbound.Extra["detour"] != "direct" {
		t.Fatalf("lost WS fields: %#v", outbound)
	}
	if value, ok := outbound.Extra["udp_over_tcp"].(map[string]any); !ok || value == nil {
		t.Fatal("empty native object lost")
	}
}
