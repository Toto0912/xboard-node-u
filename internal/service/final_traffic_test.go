package service

import (
	"context"
	"testing"
)

type finalCountersKernel struct{ fakeKernel }

func (k *finalCountersKernel) GetUserTraffic(context.Context) (map[int][2]int64, map[int]map[string]bool, int, error) {
	if k.running {
		return map[int][2]int64{7: {11, 15}}, nil, 0, nil
	}
	return map[int][2]int64{7: {100, 150}}, nil, 0, nil
}

func TestStopCapturesOnlyFinalDeltaAfterConnectionsClose(t *testing.T) {
	k := &finalCountersKernel{fakeKernel{running: true}}
	s := newTestService(&k.fakeKernel)
	s.kernel = k
	s.trackAndEnforce(context.Background())
	if got := s.tracker.FlushTraffic()[7]; got != [2]int64{11, 15} {
		t.Fatal(got)
	}
	s.stopKernel()
	s.stopKernel()
	if got := s.tracker.FlushTraffic()[7]; got != [2]int64{89, 135} {
		t.Fatal(got)
	}
	if s.tracker.ActiveConnections() != 0 || len(s.tracker.CurrentOnline()) != 0 {
		t.Fatal("stale shutdown activity")
	}
}
