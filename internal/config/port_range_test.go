package config

import "testing"

func TestHY2PortRangeCompatibility(t *testing.T) {
	for _, kernel := range []string{"singbox", "xray"} {
		for _, settings := range []*HY2Settings{nil, {}, {PortRange: "60000-65000"}} {
			if err := ValidateHY2(settings, "hysteria", 2, kernel, 0, 0); err != nil {
				t.Fatal(err)
			}
			if err := ValidateHY2PortRange(settings, 60000); err != nil {
				t.Fatal(err)
			}
		}
		settings := &HY2Settings{PortRange: "60000-65000"}
		if ValidateHY2PortRange(settings, 60001) == nil {
			t.Fatal("mismatching scalar port accepted")
		}
		if ValidateHY2(settings, "vless", 2, kernel, 0, 0) == nil {
			t.Fatal("range accepted on a non-HY2 node")
		}
		if ValidateHY2(settings, "hysteria", 1, kernel, 0, 0) == nil {
			t.Fatal("range accepted on Hysteria 1")
		}
	}
}
