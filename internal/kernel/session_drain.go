package kernel

import (
	"sync"
	"time"
)

// SessionDrain belongs to one kernel instance. Register before protocol I/O and
// finish after its accounting callbacks return, including asynchronous UDP copies.
type SessionDrain struct {
	mu      sync.Mutex
	active  map[*func()]struct{}
	drained chan struct{}
}

func (d *SessionDrain) Begin(interrupt func()) (func(), bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.drained != nil {
		return nil, false
	}
	if d.active == nil {
		d.active = make(map[*func()]struct{})
	}
	d.active[&interrupt] = struct{}{}
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if _, registered := d.active[&interrupt]; !registered {
			return
		}
		delete(d.active, &interrupt)
		if d.drained != nil && len(d.active) == 0 {
			close(d.drained)
		}
	}, true
}

// CloseAndWait rejects newly arriving sessions and interrupts existing sessions
// before their counters are sampled. A broken transport cannot hold shutdown forever.
func (d *SessionDrain) CloseAndWait() bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	if d.drained == nil {
		d.drained = make(chan struct{})
		if len(d.active) == 0 {
			close(d.drained)
		} else {
			interrupts := make([]func(), 0, len(d.active))
			for interrupt := range d.active {
				interrupts = append(interrupts, *interrupt)
			}
			go func() {
				for _, interrupt := range interrupts {
					interrupt()
				}
			}()
		}
	}
	done := d.drained
	d.mu.Unlock()
	select {
	case <-done:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}
