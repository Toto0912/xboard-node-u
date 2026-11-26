package limiter

import (
	"sync"
	"sync/atomic"

	"golang.org/x/time/rate"
)

// SpeedTrackerLogCallback is called when bucket updates occur.
type SpeedTrackerLogCallback func(msg string)

// SpeedTracker manages per-user token-bucket rate limiters.
// It does NOT wrap connections itself — instead, ConnTracker consults it
// via GetLimiter to embed rate limiting in the same tracked connection
// wrapper that does byte counting.
type SpeedTracker struct {
	limiter *Limiter
	mu      sync.RWMutex
	buckets map[int]*rate.Limiter // userID → shared rate limiter
	uuidMap map[string]int        // UUID → userID

	// Fast-path: when no users have a speed limit, GetLimiter returns nil
	// immediately without any map lookup.
	hasLimits atomic.Bool

	// Optional callback for logging
	logFunc SpeedTrackerLogCallback
}

// NewSpeedTracker creates a bucket manager for per-user bandwidth throttling.
func NewSpeedTracker(l *Limiter) *SpeedTracker {
	return &SpeedTracker{
		limiter: l,
		buckets: make(map[int]*rate.Limiter),
		uuidMap: make(map[string]int),
	}
}

// SetLogCallback sets the logging callback.
func (t *SpeedTracker) SetLogCallback(f SpeedTrackerLogCallback) {
	t.logFunc = f
}

// UpdateBuckets updates the UUID→userID mapping and syncs existing limiters.
func (t *SpeedTracker) UpdateBuckets() {

	t.mu.Lock()
	t.limiter.mu.RLock()
	uuids := make(map[string]int, len(t.limiter.users))
	active := 0
	for id, u := range t.limiter.users {
		if u.UUID != "" {
			uuids[u.UUID] = id
		}
		bucket := t.buckets[id]
		if u.SpeedLimit <= 0 {
			if bucket != nil {
				bucket.SetLimit(rate.Inf)
			}
			continue
		}
		active++
		r := rate.Limit(float64(u.SpeedLimit) * 1_000_000 / 8)
		burst := max(64*1024, int(r))
		if bucket == nil {
			t.buckets[id] = rate.NewLimiter(r, burst)
		} else {
			bucket.SetLimit(r)
			bucket.SetBurst(burst)
		}
	}
	for id := range t.buckets {
		if _, ok := t.limiter.users[id]; !ok {
			delete(t.buckets, id)
		}
	}
	t.limiter.mu.RUnlock()
	t.uuidMap = uuids
	t.hasLimits.Store(active > 0)
	t.mu.Unlock()

	if t.logFunc != nil {
		t.logFunc("buckets updated")
	}
}

// GetLimiter returns the rate limiter for the given user UUID, or nil if
// no active rate applies. Bucket handles survive temporary unlimited settings.
// Thread-safe.
func (t *SpeedTracker) GetLimiter(user string) *rate.Limiter {
	if !t.hasLimits.Load() {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	bucket := t.buckets[t.uuidMap[user]]
	if bucket != nil && bucket.Limit() != rate.Inf {
		return bucket
	}
	return nil
}

// HasLimits returns true if any user currently has a speed limit configured.
func (t *SpeedTracker) HasLimits() bool {
	return t.hasLimits.Load()
}

// LimitedUserCount returns the number of users with active speed limits.
func (t *SpeedTracker) LimitedUserCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	count := 0
	for _, bucket := range t.buckets {
		if bucket.Limit() != rate.Inf {
			count++
		}
	}
	return count
}
