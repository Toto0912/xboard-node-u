package config

import "testing"

func TestNegativeIntervalsRejected(t *testing.T) {
	for _, neg := range []func(*Config){func(c *Config) { c.Node.TrackInterval = -1 }, func(c *Config) { c.Node.DeviceReportInterval = -1 }, func(c *Config) { c.WS.DiscoveryInterval = -1 }, func(c *Config) { c.WS.StatusInterval = -1 }, func(c *Config) { c.WS.BackoffMax = -1 }} {
		c := Config{Panel: PanelConfig{URL: "https://p.invalid", Token: "token", NodeID: 1}, Kernel: KernelConfig{Type: "singbox"}}
		if e := c.validate(); e != nil {
			t.Fatal(e)
		}
		neg(&c)
		if e := c.validate(); e == nil {
			t.Fatal("accepted negative timer")
		}
	}
}

func TestExplicitEmptyBindingsStayEmptyWithLegacyEnv(t *testing.T) {
	t.Setenv("PANEL_URL", "https://old.invalid")
	p := writeTemp(t, "instances: []\npanel:\n  url: https://old.invalid\n  node_id: 1\n  token: old\n")
	c, e := LoadRoot(p)
	if e != nil {
		t.Fatal(e)
	}
	rows, e := c.NormalizeInstances()
	if e != nil || len(rows) != 0 {
		t.Fatalf("empty bindings yielded %d, %v", len(rows), e)
	}
}
