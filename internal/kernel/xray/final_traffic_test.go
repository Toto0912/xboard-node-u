package xray

import (
	"context"
	"testing"

	"github.com/cedar2025/xboard-node/internal/model"
)

func TestStopRetainsUnsampledAndRemovedAccountTraffic(t *testing.T) {
	x, mgr := newStatsBackedXray(t, []model.UserSpec{{ID: 1}}, nil)
	up := mustRegisterCounter(t, mgr, counterName(1, "uplink"))
	up.Add(17)
	if _, _, _, err := x.GetUserTraffic(context.Background()); err != nil {
		t.Fatal(err)
	}
	up.Add(35)
	mustRegisterCounter(t, mgr, counterName(1, "downlink")).Add(101)
	mustRegisterCounter(t, mgr, counterName(2, "uplink")).Add(19)
	x.Stop()
	for i := 0; i < 2; i++ {
		rows, _, _, err := x.GetUserTraffic(context.Background())
		if err != nil || rows[1] != [2]int64{52, 101} || rows[2] != [2]int64{19, 0} {
			t.Fatalf("stopped counters = %v, err = %v", rows, err)
		}
		x.Stop()
	}
}
