package config

import (
	"encoding/json"
	"testing"
)

func TestHY2ValidationAndClone(t *testing.T) {
	for _, core := range []string{"singbox", "xray"} {
		for _, kind := range []string{"file", "proxy", "string"} {
			m := map[string]any{"type": kind}
			dir, status, host := "directory", "status_code", "rewrite_host"
			if core == "xray" {
				dir, status, host = "dir", "statusCode", "rewriteHost"
			}
			switch kind {
			case "file":
				m[dir] = "/tmp/site"
			case "proxy":
				m["url"], m[host] = "https://example.com", false
			case "string":
				m["content"], m[status], m["headers"] = "ok", 200, map[string]any{"Content-Type": "text/plain"}
			}
			s := &HY2Settings{BandwidthMode: "auto", Kernel: core, Masquerade: m}
			if err := ValidateHY2(s, "hysteria", 2, core, 23, 47); err != nil {
				t.Fatal(err)
			}
			copy := s.Clone()
			copy.Masquerade["type"] = "changed"
			if s.Masquerade["type"] != kind {
				t.Fatal("clone shares map")
			}
			bytes, _ := json.Marshal(s)
			var decoded HY2Settings
			if err := json.Unmarshal(bytes, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := ValidateHY2(&decoded, "hysteria", 2, core, 0, 0); err != nil {
				t.Fatal(err)
			}
			m["quic"] = map[string]any{}
			if ValidateHY2(s, "hysteria", 2, core, 0, 0) == nil {
				t.Fatal("unsupported field accepted")
			}
		}
	}
	if err := ValidateHY2(nil, "hysteria", 2, "singbox", 0, 0); err != nil {
		t.Fatal(err)
	}
	if (&HY2Settings{}).AutoBandwidth() || (*HY2Settings)(nil).AutoBandwidth() {
		t.Fatal("legacy config opted into new policy")
	}
	if ValidateHY2(&HY2Settings{BandwidthMode: "auto"}, "hysteria", 1, "singbox", 0, 0) == nil {
		t.Fatal("HY1 extension accepted")
	}
	if ValidateHY2(nil, "hysteria", 2, "xray", -1, 0) == nil {
		t.Fatal("negative bandwidth accepted")
	}
}

func TestHY2RejectInvalidMasquerade(t *testing.T) {
	for _, raw := range []string{
		`{"type":"proxy","url":"file:///etc/passwd"}`,
		`{"type":"proxy","url":"https://user:pass@example.com"}`,
		`{"type":"proxy","url":"https://example.com","insecure":"false"}`,
		`{"type":"string","content":"ok","statusCode":200.5}`,
		`{"type":"string","content":"ok","headers":{"X-Test":"a\r\nb"}}`,
		`{"type":"string","content":"ok","headers":{"Content-Type":["text/plain"]}}`,
	} {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if ValidateHY2(&HY2Settings{Kernel: "xray", Masquerade: m}, "hysteria", 2, "xray", 0, 0) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
