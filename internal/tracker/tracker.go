package tracker

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

// snapshot is an immutable point-in-time view of tracker state.
// It is swapped atomically so readers never block writers.
type snapshot struct {
	aliveIPs  map[int]map[string]bool // userID → set of source IPs
	online    map[int]int             // userID → distinct IP count
	connCount int
	inSpeed   int64
	outSpeed  int64
}

var emptySnapshot snapshot

// Tracker computes per-user traffic deltas from cumulative counters
// provided by the kernel, and accumulates totals for panel reporting.
//
// Architecture: the kernel maintains per-user atomic counters and IP sets.
// Each tick, the service calls Process() with cumulative per-user traffic.
// Tracker computes deltas against the previous cycle's values — O(users),
// not O(connections).
//
// Thread safety: Process() acquires mu to update internal state, then
// atomically publishes a new snapshot. All read methods (Flush*,
// CurrentOnline, LogStats, *Speed) read the snapshot lock-free.
// This eliminates contention between the 10s Process tick and the 60s
// flush/push tick.
type Tracker struct {
	mu sync.Mutex

	// lastSeen stores the cumulative traffic from the previous Process() call.
	// Protected by mu — only written by Process.
	lastSeen  map[int][2]int64 // userID → [upload, download] cumulative
	sampledAt time.Time        // last successful sample in this run, protected by mu

	// pending accumulates deltas between flushes.
	// Protected by mu — written by Process, drained by FlushTraffic.
	pendingTraffic map[int][2]int64

	// live holds the current snapshot, swapped atomically.
	// Readers load this pointer without any lock.
	live atomic.Pointer[snapshot]
}

func New() *Tracker {
	t := &Tracker{
		lastSeen:       make(map[int][2]int64),
		pendingTraffic: make(map[int][2]int64),
	}
	t.ClearActivity()
	return t
}

// ClearActivity publishes zero connections, devices and speed. Both kernels
// retain cumulative traffic across restarts, so lastSeen and unsent deltas
// must survive stops, including a stop while a report is in flight.
func (t *Tracker) ClearActivity() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sampledAt = time.Time{}
	t.live.Store(&emptySnapshot)
}

// Process computes per-user traffic deltas from cumulative kernel counters.
// Also stores alive IPs and connection count. O(users).
//
// This is the only writer to lastSeen and pendingTraffic.
// After computing deltas, it publishes a new snapshot for lock-free reads.
func (t *Tracker) Process(
	cumTraffic map[int][2]int64,
	kernelAliveIPs map[int]map[string]bool,
	connCount int,
) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()

	var cycleIn, cycleOut int64

	for uid, cum := range cumTraffic {
		prev := t.lastSeen[uid]
		deltaUp := cum[0] - prev[0]
		deltaDown := cum[1] - prev[1]

		// Guard against counter reset (kernel restart).
		if deltaUp < 0 {
			deltaUp = cum[0]
		}
		if deltaDown < 0 {
			deltaDown = cum[1]
		}

		t.lastSeen[uid] = cum

		if deltaUp > 0 || deltaDown > 0 {
			cur := t.pendingTraffic[uid]
			cur[0] += deltaUp
			cur[1] += deltaDown
			t.pendingTraffic[uid] = cur

			cycleOut += deltaUp
			cycleIn += deltaDown
		}
	}

	// Compute online from alive IPs.
	online := make(map[int]int, len(kernelAliveIPs))
	for uid, ips := range kernelAliveIPs {
		online[uid] = len(ips)
	}

	s := &snapshot{
		aliveIPs:  kernelAliveIPs, // kernel provides fresh copy each tick
		online:    online,
		connCount: connCount,
	}
	if elapsed := now.Sub(t.sampledAt).Seconds(); !t.sampledAt.IsZero() && elapsed > 0 {
		s.inSpeed = int64(float64(cycleIn) / elapsed)
		s.outSpeed = int64(float64(cycleOut) / elapsed)
	}
	t.sampledAt = now
	t.live.Store(s)
}

// FlushTraffic returns accumulated per-user traffic and resets the pending buffer.
// Lock-free: reads from live snapshot, then acquires mu only to drain.
func (t *Tracker) FlushTraffic() map[int][2]int64 {
	t.mu.Lock()
	data := t.pendingTraffic
	t.pendingTraffic = make(map[int][2]int64, len(data))
	t.mu.Unlock()
	return data
}

// RestoreTraffic adds traffic back (used when push to panel fails).
func (t *Tracker) RestoreTraffic(data map[int][2]int64) {
	t.mu.Lock()
	for uid, d := range data {
		cur := t.pendingTraffic[uid]
		cur[0] += d[0]
		cur[1] += d[1]
		t.pendingTraffic[uid] = cur
	}
	t.mu.Unlock()
}

// HasTraffic returns true if there is accumulated traffic to report.
func (t *Tracker) HasTraffic() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pendingTraffic) > 0
}

// FlushAliveIPs returns an independently owned, complete device snapshot.
// Report unchanged devices too: the panel expires their state after five minutes.
// Each HTTP/WS report can safely retain this value while later ticks run.
func (t *Tracker) FlushAliveIPs() map[int][]string {
	s := t.live.Load()
	devices := make(map[int][]string, len(s.aliveIPs))
	for uid, ips := range s.aliveIPs {
		list := make([]string, 0, len(ips))
		for ip := range ips {
			list = append(list, ip)
		}
		sort.Strings(list)
		devices[uid] = list
	}
	return devices
}

// CurrentOnline returns a snapshot copy of user_id → device count (distinct IPs).
// Lock-free: reads from live snapshot.
func (t *Tracker) CurrentOnline() map[int]int {
	s := t.live.Load()
	cp := make(map[int]int, len(s.online))
	for k, v := range s.online {
		cp[k] = v
	}
	return cp
}

// LogStats logs current tracking statistics.
// Lock-free: reads from live snapshot.
func (t *Tracker) LogStats() {
	s := t.live.Load()
	nlog.TrackerStats(s.connCount, len(s.online))
}

// ActiveConnections returns the last observed active connection count.
// Lock-free: reads from live snapshot.
func (t *Tracker) ActiveConnections() int {
	return t.live.Load().connCount
}

// TotalConnections is deprecated — no longer tracked per-connection.
// Returns 0 for backward compatibility.
func (t *Tracker) TotalConnections() int64 {
	return 0
}

// InboundSpeed returns the last observed inbound (download) speed in bytes/second.
// Lock-free: reads from live snapshot.
func (t *Tracker) InboundSpeed() int64 {
	return t.live.Load().inSpeed
}

// OutboundSpeed returns the last observed outbound (upload) speed in bytes/second.
// Lock-free: reads from live snapshot.
func (t *Tracker) OutboundSpeed() int64 {
	return t.live.Load().outSpeed
}
