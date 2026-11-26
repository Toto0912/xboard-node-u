package limiter

import (
	"github.com/cedar2025/xboard-node/internal/model"
	"golang.org/x/time/rate"
	"testing"
)

func TestEstablishedBucketReleasesAndReapplies(t *testing.T) {
	l := New()
	st := NewSpeedTracker(l)
	set := func(n int) { l.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "u", SpeedLimit: n}}); st.UpdateBuckets() }
	set(8)
	b := st.GetLimiter("u")
	if b == nil {
		t.Fatal("missing limit")
	}
	set(1)
	if b.Limit() != 125000 {
		t.Fatal("not lowered")
	}
	set(0)
	if b.Limit() != rate.Inf || st.GetLimiter("u") != nil {
		t.Fatal("still throttled")
	}
	set(2)
	if st.GetLimiter("u") != b || b.Limit() != 250000 {
		t.Fatal("old bucket stale")
	}
	l.UpdateUsers(nil)
	st.UpdateBuckets()
	if st.GetLimiter("u") != nil {
		t.Fatal("removed account retained")
	}
}
