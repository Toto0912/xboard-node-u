package monitor

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestConcurrentNodesShareFreshHostSample(t *testing.T) {
	var samples [16]Status
	var group sync.WaitGroup
	for i := range samples {
		group.Add(1)
		go func() {
			defer group.Done()
			samples[i] = Collect()
		}()
	}
	group.Wait()
	for _, other := range samples[1:] {
		if !reflect.DeepEqual(samples[0], other) {
			t.Fatal("co-located nodes received different host samples")
		}
	}
	if len(samples[0].CPUPerCore) > 0 {
		samples[0].CPUPerCore[0] = -100
		if Collect().CPUPerCore[0] == -100 {
			t.Fatal("caller changed shared CPU counters")
		}
	}
	hostSample.Lock()
	hostSample.status.Goroutines = -1
	hostSample.expires = time.Time{}
	hostSample.Unlock()
	if Collect().Goroutines <= 0 {
		t.Fatal("expired snapshot was not refreshed")
	}
}
