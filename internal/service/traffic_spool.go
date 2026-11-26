package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/tracker"
	"github.com/google/uuid"
)

const (
	maxBatchUsers = 1000
	maxBatchFiles = 128
	maxBatchBytes = 128 << 10
)

var errJournalFull = errors.New("traffic journal is full; remaining usage retained in memory")

type trafficSpool struct {
	dir     string
	writeMu *sync.Mutex // shared by the periodic reporter and the final local checkpoint
}

type spoolStatus struct {
	Pending int
	Oldest  time.Time
}

func (q trafficSpool) status() (*spoolStatus, error) {
	entries, err := os.ReadDir(q.dir)
	if errors.Is(err, os.ErrNotExist) {
		return &spoolStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	status := new(spoolStatus)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		status.Pending++
		if status.Oldest.IsZero() {
			prefix, _, _ := strings.Cut(entry.Name(), "-")
			if nanos, err := strconv.ParseInt(prefix, 10, 64); err == nil {
				status.Oldest = time.Unix(0, nanos)
			}
		}
	}
	return status, nil
}

func newTrafficSpool(cfg *config.Config) trafficSpool {
	// Tokens and machine credentials may rotate without changing a node's ledger.
	identity := fmt.Sprintf("%s\n%d", strings.TrimRight(cfg.Panel.URL, "/"), cfg.Panel.NodeID)
	root := os.Getenv("XBOARD_NODE_TRAFFIC_DIR")
	if root == "" {
		root = filepath.Join(cfg.Kernel.ConfigDir, "traffic-batches")
	}
	return trafficSpool{dir: filepath.Join(root, fmt.Sprintf("%x", sha256.Sum256([]byte(identity))))}
}

func (q trafficSpool) exists() bool {
	_, err := os.Stat(q.dir)
	return !errors.Is(err, os.ErrNotExist) // an inaccessible journal must not select legacy billing
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (q trafficSpool) prepare() error {
	if err := os.MkdirAll(q.dir, 0700); err != nil {
		return err
	}
	// Persist both newly created directory entries before sending any new UUID.
	root := filepath.Dir(q.dir)
	if err := syncDir(filepath.Dir(root)); err != nil {
		return err
	}
	return syncDir(root)
}

// list returns pending batches in creation order; files from interrupted writes were never sent.
func (q trafficSpool) list() ([]string, int, error) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return nil, 0, err
	}
	var names []string
	partial := 0
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".json"):
			names = append(names, e.Name())
		case strings.HasPrefix(e.Name(), ".partial-"):
			info, err := e.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, 0, err
			}
			if time.Since(info.ModTime()) > time.Hour {
				if err := os.Remove(filepath.Join(q.dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, 0, err
				}
			} else {
				partial++
			}
		default:
			return nil, 0, errors.New("unrecognized traffic journal entry; leaving journal intact")
		}
	}
	return names, partial, nil
}

// Once rename succeeds the immutable file owns these rows, including when a later directory fsync fails.
func (q trafficSpool) put(rows [][3]int64) error {
	id, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	b := panel.TrafficBatch{ID: id.String(), Traffic: rows}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(q.dir, ".partial-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	name := fmt.Sprintf("%020d-%s-%x.json", time.Now().UnixNano(), b.ID, sha256.Sum256(data))
	return os.Rename(f.Name(), filepath.Join(q.dir, name))
}

// Freeze one report window. Only rows that have never become durable go back into the tracker.
func (q trafficSpool) freeze(t *tracker.Tracker, available int) error {
	if !t.HasTraffic() {
		return nil
	}
	if available <= 0 {
		return errJournalFull
	}
	deltas := t.FlushTraffic()
	defer t.RestoreTraffic(deltas)
	ids := make([]int, 0, len(deltas))
	for id, delta := range deltas {
		if id <= 0 || int64(id) > math.MaxInt32 || delta[0] < 0 || delta[1] < 0 || delta == [2]int64{} {
			return errors.New("invalid sampled traffic; retained in memory")
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for len(ids) > 0 && available > 0 {
		rows := make([][3]int64, 0, min(len(ids), maxBatchUsers))
		var up, down int64
		for _, id := range ids {
			d := deltas[id]
			if len(rows) == maxBatchUsers || up > math.MaxInt64-d[0] || down > math.MaxInt64-d[1] {
				break
			}
			rows = append(rows, [3]int64{int64(id), d[0], d[1]})
			up += d[0]
			down += d[1]
		}
		if err := q.put(rows); err != nil {
			return err
		}
		for _, row := range rows {
			delete(deltas, int(row[0]))
		}
		ids = ids[len(rows):]
		available--
	}
	if len(ids) != 0 {
		return errJournalFull
	}
	return nil
}

func (q trafficSpool) read(name string) (panel.TrafficBatch, error) {
	var b panel.TrafficBatch
	f, err := os.Open(filepath.Join(q.dir, name))
	if err != nil {
		return b, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBatchBytes+1))
	if err != nil {
		return b, err
	}
	if len(data) > maxBatchBytes || json.Unmarshal(data, &b) != nil ||
		!strings.HasSuffix(name, fmt.Sprintf("-%s-%x.json", b.ID, sha256.Sum256(data))) {
		return b, errors.New("corrupt traffic journal entry; file retained for recovery")
	}
	return b, nil
}

// drain leaves item-specific failures in place and stops at the first transport failure.
func (q trafficSpool) drain(ctx context.Context, client *panel.Client, names []string) (freed int, itemErr, stopErr error) {
	for _, name := range names {
		b, err := q.read(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			itemErr = errors.Join(itemErr, err)
			continue
		}
		err = client.SendTrafficBatch(ctx, b)
		if errors.Is(err, panel.ErrBatchRejected) {
			itemErr = errors.Join(itemErr, err)
			continue
		}
		if err != nil {
			return freed, itemErr, err
		}
		if err := os.Remove(filepath.Join(q.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return freed, itemErr, err
		}
		freed++
	}
	return freed, itemErr, nil
}

func (q trafficSpool) checkpoint(t *tracker.Tracker) error {
	if q.writeMu != nil {
		q.writeMu.Lock()
		defer q.writeMu.Unlock()
	}
	if err := q.prepare(); err != nil {
		return err
	}
	names, partial, err := q.list()
	if err != nil {
		return err
	}
	err = q.freeze(t, maxBatchFiles-len(names)-partial)
	return errors.Join(err, syncDir(q.dir))
}

func (q trafficSpool) report(ctx context.Context, client *panel.Client, t *tracker.Tracker, freeze bool) error {
	// Idle, negotiated nodes need no disk writes. Preserve the directory as the durable protocol marker.
	if freeze && !t.HasTraffic() {
		names, _, err := q.list()
		if err == nil && len(names) == 0 {
			return nil
		}
	}
	for {
		var freezeErr error
		if freeze {
			freezeErr = q.checkpoint(t)
		} else if err := q.prepare(); err != nil {
			return err
		}
		names, partial, err := q.list()
		if err != nil {
			return errors.Join(freezeErr, err)
		}
		if err := syncDir(q.dir); err != nil {
			return errors.Join(freezeErr, err)
		}
		freed, itemErr, stopErr := q.drain(ctx, client, names)
		if freed > 0 {
			if err := syncDir(q.dir); err != nil {
				return errors.Join(freezeErr, itemErr, stopErr, err)
			}
			if errors.Is(freezeErr, errJournalFull) {
				// Even at shutdown or deadline, use the slots just released to persist remaining samples.
				if stopErr != nil || ctx.Err() != nil {
					freezeErr = q.checkpoint(t)
					return errors.Join(freezeErr, itemErr, stopErr, ctx.Err(), syncDir(q.dir))
				}
				continue
			}
		}
		if err := errors.Join(freezeErr, itemErr, stopErr); err != nil {
			return err
		}
		if !freeze && partial == 0 {
			if err := os.Remove(q.dir); err != nil {
				return err
			}
			return syncDir(filepath.Dir(q.dir))
		}
		return nil
	}
}
