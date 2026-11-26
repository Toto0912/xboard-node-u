package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cedar2025/xboard-node/internal/model"
)

type rateSampleKernel struct {
	fakeKernel
	counters [2]int64
	readErr  error
}

func (k *rateSampleKernel) GetUserTraffic(context.Context) (map[int][2]int64, map[int]map[string]bool, int, error) {
	return map[int][2]int64{7: k.counters}, nil, 0, k.readErr
}

func TestTrackingSpeedUsesSuccessfulSampleInterval(t *testing.T) {
	for _, interval := range []time.Duration{10 * time.Second, 30 * time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				k := &rateSampleKernel{}
				s := newTestService(&k.fakeKernel)
				s.kernel = k
				spec := &model.NodeSpec{Protocol: "vless"}
				users := []model.UserSpec{{ID: 7, UUID: "user-7"}}
				start := func() {
					t.Helper()
					if !s.startKernel(spec, users) {
						t.Fatal("kernel start failed")
					}
				}
				checkSpeed := func(up, down int64) {
					t.Helper()
					if gotUp, gotDown := s.tracker.OutboundSpeed(), s.tracker.InboundSpeed(); gotUp != up || gotDown != down {
						t.Fatalf("upload/download = %d/%d B/s, want %d/%d", gotUp, gotDown, up, down)
					}
				}
				const upload, download = int64(1200), int64(3400)
				sample := func(duration time.Duration) {
					time.Sleep(duration)
					k.counters[0] += upload * int64(duration) / int64(time.Second)
					k.counters[1] += download * int64(duration) / int64(time.Second)
					s.trackAndEnforce(context.Background())
				}

				time.Sleep(time.Minute) // Setup before the kernel first accepted traffic.
				start()
				checkSpeed(0, 0)
				sample(interval)
				checkSpeed(upload, download)

				k.readErr = errors.New("sample unavailable")
				sample(interval)
				k.readErr = nil
				sample(interval + 500*time.Millisecond)
				checkSpeed(upload, download)
				if got := s.tracker.FlushTraffic()[7]; got != k.counters {
					t.Fatalf("accounted bytes = %v, want %v", got, k.counters)
				}

				s.stopKernel()
				checkSpeed(0, 0)
				time.Sleep(time.Hour)
				start()
				checkSpeed(0, 0)
				sample(time.Second)
				checkSpeed(upload, download)
				if got := s.tracker.FlushTraffic()[7]; got != [2]int64{upload, download} {
					t.Fatalf("accounted bytes after restart = %v", got)
				}
				time.Sleep(interval)
				s.trackAndEnforce(context.Background())
				checkSpeed(0, 0)
			})
		})
	}
}
