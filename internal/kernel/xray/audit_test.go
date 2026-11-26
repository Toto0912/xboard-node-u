package xray

import (
	"context"
	"encoding/json"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"testing"
)

func TestDeviceReferencesAcrossLimitTransitions(t *testing.T) {
	d := newTestDispatcher()
	e := userEmail(17)
	ids := map[string]int{e: 17}
	d.UpdateLimits(ids, nil, nil)
	if d.checkDeviceLimit(e, "127.0.0.1", true) {
		t.Fatal("first denied")
	}
	d.UpdateLimits(ids, map[string]int{e: 1}, nil)
	if !d.checkDeviceLimit(e, "127.0.0.2", true) {
		t.Fatal("expected occupied")
	}
	d.delConn(e, "127.0.0.1")
	if d.checkDeviceLimit(e, "127.0.0.2", true) {
		t.Fatal("slot leaked")
	}
	d.UpdateLimits(ids, nil, nil)
	d.delConn(e, "127.0.0.2")
	ips, _ := d.GetConnectionState()
	if len(ips) != 0 {
		t.Fatalf("stale %v", ips)
	}
}
func TestRemovedUsersCountersDrainExactlyOnce(t *testing.T) {
	x, mgr := newStatsBackedXray(t, []model.UserSpec{{ID: 1, UUID: "one"}, {ID: 2, UUID: "two"}}, newTestDispatcher())
	c := mustRegisterCounter(t, mgr, counterName(1, "uplink"))
	c.Add(17)
	x.users = x.users[1:]
	for i, want := range []int64{17, 22, 22} {
		if i == 1 {
			c.Add(5)
		}
		tr, _, _, err := x.GetUserTraffic(context.Background())
		if err != nil || tr[1][0] != want {
			t.Fatalf("%v %v", tr, err)
		}
	}
}
func TestXrayBuilderLeavesNodeUnchanged(t *testing.T) {
	n := &model.NodeSpec{Protocol: "trojan", Network: "xhttp", ServerPort: 12345, NetworkSettings: map[string]any{"extra": map[string]any{"nested": map[string]any{"sockopt": []any{}}}}}
	before, _ := json.Marshal(n)
	a := buildInbound(n, nil, kernel.TLSCert{})
	after, _ := json.Marshal(n)
	if string(before) != string(after) || a["streamSettings"].(M)["sockopt"].(M)["reusePort"] != true {
		t.Fatal("configuration mutated or socket options lost")
	}
	if _, err := marshalConfig(config.KernelConfig{}, &model.NodeSpec{Protocol: "unknown"}, nil, kernel.TLSCert{}); err == nil {
		t.Fatal("missing inbound accepted")
	}
}
