package service

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/tracker"
)

func TestTrafficBatchReceiptLossRestartAndTelemetry(t *testing.T) {
	for _, machine := range []bool{false, true} {
		mode := "direct"
		if machine {
			mode = "machine"
		}
		t.Run(mode, func(t *testing.T) {
			var phase atomic.Int32
			cfg := config.PanelConfig{NodeID: 17, Token: "one"}
			if machine {
				cfg.MachineID = 4
			}
			type request struct {
				panel.TrafficBatch
				Token     string `json:"token"`
				NodeID    int    `json:"node_id"`
				MachineID int    `json:"machine_id"`
			}
			batches := make(chan request, 16)
			reports := make(chan map[string]json.RawMessage, 16)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/server/handshake", "/api/v2/server/machine/nodes":
					switch phase.Load() {
					case 0:
						w.Write([]byte(`{"nodes":[],"capabilities":{"traffic_batch_v1":true}}`))
					case 4:
						w.Write([]byte(`{"nodes":[],"capabilities":{"traffic_batch_v1":false}}`))
					default:
						w.Write([]byte(`{"nodes":[]}`))
					}
				case "/api/v2/server/traffic-batch":
					var b request
					if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
						t.Error(err)
					}
					if b.NodeID != 17 || b.MachineID != cfg.MachineID {
						t.Errorf("wrong billing identity %+v", b)
					}
					batches <- b
					switch phase.Load() {
					case 0: // received and committed, connection closes before the ACK
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
					case 1:
						w.WriteHeader(404)
					case 2:
						w.Write([]byte(`{"batch_id":"late-ack"}`))
					default:
						json.NewEncoder(w).Encode(map[string]string{"batch_id": b.ID})
					}
				case "/api/v2/server/report":
					var report map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
						t.Error(err)
					}
					reports <- report
					w.Write([]byte("{}"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			cfg.URL = srv.URL
			dir := t.TempDir()
			makeNode := func() *Service {
				s := newTestService(&fakeKernel{})
				s.cfg.Panel = cfg
				s.cfg.Kernel.ConfigDir = dir
				if machine {
					root := panel.NewClient(cfg)
					child := root.ForNode(cfg.NodeID)
					if _, err := root.GetMachineNodes(); err != nil {
						t.Fatal(err)
					}
					cp := controlplane.NewMachinePanelControlPlane(child, config.KernelConfig{}, nil, nil)
					s.sink, s.source = cp, cp
				} else {
					cp := controlplane.NewPanelControlPlane(cfg, config.WSConfig{}, config.KernelConfig{})
					if _, err := cp.BatchClient().Handshake(); err != nil {
						t.Fatal(err)
					}
					s.sink, s.source = cp, cp
				}
				return s
			}
			push := func(s *Service, legacy bool) {
				t.Helper()
				s.pushReportSync(5 * time.Second)
				select {
				case row := <-reports:
					if _, ok := row["traffic"]; ok != legacy || row["alive"] == nil {
						t.Fatalf("unexpected telemetry %v", row)
					}
				default:
					t.Fatal("missing independent telemetry")
				}
			}
			read := func() request {
				t.Helper()
				select {
				case b := <-batches:
					return b
				default:
					t.Fatal("missing batch")
					return request{}
				}
			}
			s := makeNode()
			s.tracker.Process(map[int][2]int64{2: {11, 17}, 1: {120, 340}}, nil, 0)
			push(s, false)
			first := read()
			if !reflect.DeepEqual(first.Traffic, [][3]int64{{1, 120, 340}, {2, 11, 17}}) {
				t.Fatal(first)
			}
			phase.Store(1)
			cfg.Token = "rotated"
			s = makeNode() // after process exit, old panel advertises no capability
			s.tracker.Process(map[int][2]int64{3: {5, 7}}, nil, 0)
			push(s, false)
			again := read()
			if again.ID != first.ID || !reflect.DeepEqual(again.Traffic, first.Traffic) || again.Token != "rotated" {
				t.Fatal("changed retry", again)
			}
			phase.Store(2)
			push(s, false)
			if read().ID != first.ID {
				t.Fatal("old 404 released batch")
			}
			phase.Store(3)
			push(s, false)
			if read().ID != first.ID {
				t.Fatal("wrong ack released batch")
			}
			second := read()
			if second.ID == first.ID || !reflect.DeepEqual(second.Traffic, [][3]int64{{3, 5, 7}}) {
				t.Fatal("new traffic not isolated", second)
			}
			push(s, false)
			if len(batches) != 0 {
				t.Fatal("acknowledged batch replayed")
			}
			phase.Store(4)
			s = makeNode()
			s.tracker.Process(map[int][2]int64{1: {6, 9}}, nil, 0)
			push(s, true)
			if newTrafficSpool(s.cfg).exists() {
				t.Fatal("drained opt-out persisted")
			}
		})
	}
}

func TestTrafficSpoolBoundariesAndDamage(t *testing.T) {
	cfg := &config.Config{}
	cfg.Kernel.ConfigDir = t.TempDir()
	cfg.Panel = config.PanelConfig{URL: "https://panel.one", NodeID: 1}
	q := newTrafficSpool(cfg)
	cfg.Panel.URL = "https://panel.two"
	if newTrafficSpool(cfg) == q {
		t.Fatal("different panels share a journal")
	}
	cfg.Panel.URL = "https://panel.one"
	cfg.Panel.NodeID = 2
	if newTrafficSpool(cfg) == q {
		t.Fatal("different subnodes share a journal")
	}
	pin := tracker.New()
	all := map[int][2]int64{}
	for i := 1; i <= 1005; i++ {
		all[i] = [2]int64{7, 8}
	}
	pin.Process(all, nil, 0)
	// Produce an actual ENOTDIR failure; all unsent bytes must survive.
	if err := os.WriteFile(filepath.Dir(q.dir), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := q.report(context.Background(), nil, pin, true); err == nil || !pin.HasTraffic() {
		t.Fatal("write error lost data")
	}
	if err := os.Remove(filepath.Dir(q.dir)); err != nil {
		t.Fatal(err)
	}
	if err := q.prepare(); err != nil {
		t.Fatal(err)
	}
	if err := q.freeze(pin, 1); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatal("capacity limit missing", err)
	}
	if err := q.freeze(pin, 1); err != nil || pin.HasTraffic() {
		t.Fatal("remaining rows", err)
	}
	names, _, err := q.list()
	if err != nil || len(names) != 2 {
		t.Fatal(names, err)
	}
	first, err := q.read(names[0])
	if err != nil || len(first.Traffic) != 1000 || first.Traffic[999][0] != 1000 {
		t.Fatal(err)
	}
	second, err := q.read(names[1])
	if err != nil || len(second.Traffic) != 5 || second.Traffic[0][0] != 1001 {
		t.Fatal(err)
	}
	p := filepath.Join(q.dir, names[0])
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(data), "[1,7,8]", "[1,8,8]", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := q.read(names[0]); err == nil {
		t.Fatal("valid JSON with altered counters accepted")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request panel.TrafficBatch
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.ID != second.ID {
			t.Errorf("unexpected recovered batch %s", request.ID)
		}
		json.NewEncoder(w).Encode(map[string]string{"batch_id": request.ID})
	}))
	defer srv.Close()
	recovered := trafficSpool{dir: q.dir}
	if err := recovered.report(context.Background(), panel.NewClient(config.PanelConfig{URL: srv.URL}), tracker.New(), true); err == nil {
		t.Fatal("corruption went unnoticed")
	}
	stats, err := recovered.status()
	if err != nil || stats.Pending != 1 || stats.Oldest.IsZero() {
		t.Fatalf("expected only damaged file to remain; got %+v, %v", stats, err)
	}
}

func TestTrafficSpoolConflictDoesNotBlockUnrelatedReceipts(t *testing.T) {
	q := trafficSpool{dir: filepath.Join(t.TempDir(), "traffic-batches", "node")}
	if err := q.prepare(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2, 3} {
		if err := q.put([][3]int64{{id, 1, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b panel.TrafficBatch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		switch b.Traffic[0][0] {
		case 1:
			w.WriteHeader(409)
		case 2:
			w.WriteHeader(422)
		default:
			json.NewEncoder(w).Encode(map[string]string{"batch_id": b.ID})
		}
	}))
	defer srv.Close()
	if err := q.report(context.Background(), panel.NewClient(config.PanelConfig{URL: srv.URL}), tracker.New(), true); err == nil {
		t.Fatal("missing item-level errors")
	}
	names, _, err := q.list()
	if err != nil || len(names) != 2 {
		t.Fatal(names, err)
	}
	for i, name := range names {
		b, err := q.read(name)
		if err != nil || b.Traffic[0][0] != int64(i+1) {
			t.Fatal(b, err)
		}
	}
}

func TestTrafficBatchCheckpointPrecedesBlockedShutdown(t *testing.T) {
	done := make(chan struct{})
	telemetry := make(chan struct{}, 1)
	billing := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/handshake":
			w.Write([]byte(`{"capabilities":{"traffic_batch_v1":true}}`))
		case "/api/v2/server/report":
			telemetry <- struct{}{}
			<-done
		case "/api/v2/server/traffic-batch":
			var b panel.TrafficBatch
			json.NewDecoder(r.Body).Decode(&b)
			billing <- struct{}{}
			<-done
			json.NewEncoder(w).Encode(map[string]string{"batch_id": b.ID})
		}
	}))
	defer srv.Close()
	s := newTestService(&fakeKernel{})
	s.cfg.Panel = config.PanelConfig{URL: srv.URL, NodeID: 2}
	s.cfg.Kernel.ConfigDir = t.TempDir()
	cp := controlplane.NewPanelControlPlane(s.cfg.Panel, config.WSConfig{}, config.KernelConfig{})
	if _, err := cp.BatchClient().Handshake(); err != nil {
		t.Fatal(err)
	}
	s.sink, s.source = cp, cp
	s.tracker.Process(map[int][2]int64{1: {2, 3}}, nil, 0)
	s.pushReportAsync()
	defer func() { close(done); <-s.reportDone; <-s.batchDone }()
	for _, started := range []chan struct{}{telemetry, billing} {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("missing request")
		}
	}
	s.tracker.Process(map[int][2]int64{1: {4, 6}}, nil, 0)
	s.pushReportSync(30 * time.Millisecond)
	q := newTrafficSpool(s.cfg)
	names, _, err := q.list()
	if err != nil || len(names) != 2 || s.tracker.HasTraffic() {
		t.Fatalf("shutdown checkpoint = %v %v", names, err)
	}
	for _, name := range names {
		b, err := q.read(name)
		if err != nil || !reflect.DeepEqual(b.Traffic, [][3]int64{{1, 2, 3}}) {
			t.Fatal(b, err)
		}
	}
}

func TestTrafficSpoolFullAtShutdownUsesReleasedSlots(t *testing.T) {
	q := trafficSpool{dir: filepath.Join(t.TempDir(), "traffic-batches", "node")}
	if err := q.prepare(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= maxBatchFiles; i++ {
		if err := q.put([][3]int64{{int64(i), 1, 2}}); err != nil {
			t.Fatal(err)
		}
	}
	fresh := tracker.New()
	fresh.Process(map[int][2]int64{777: {8, 9}}, nil, 0)
	var requests atomic.Int32
	var final atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b panel.TrafficBatch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		requests.Add(1)
		if reflect.DeepEqual(b.Traffic, [][3]int64{{777, 8, 9}}) {
			final.Store(true)
		}
		json.NewEncoder(w).Encode(map[string]string{"batch_id": b.ID})
	}))
	defer srv.Close()
	if err := q.report(context.Background(), panel.NewClient(config.PanelConfig{URL: srv.URL}), fresh, true); err != nil {
		t.Fatal(err)
	}
	names, _, err := q.list()
	if err != nil || len(names) != 0 || fresh.HasTraffic() || !final.Load() || int(requests.Load()) != maxBatchFiles+1 {
		t.Fatal("full journal did not drain fresh shutdown samples", names, requests.Load(), err)
	}
}

func TestTrafficSpoolSharedVolumeIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "traffic-batches")
	t.Setenv("XBOARD_NODE_TRAFFIC_DIR", root)
	cfg := &config.Config{}
	cfg.Panel = config.PanelConfig{URL: "https://panel-a.example/api", NodeID: 4, Token: "old"}
	first := newTrafficSpool(cfg)
	if filepath.Dir(first.dir) != root {
		t.Fatal(first.dir)
	}
	cfg.Kernel.ConfigDir = "/different/container/config"
	cfg.Panel.Token = "rotated"
	cfg.Panel.MachineID = 5
	if first != newTrafficSpool(cfg) {
		t.Fatal("container path, machine authentication or token changed node identity")
	}
	cfg.Panel.URL = "https://panel-b.example/api"
	if first == newTrafficSpool(cfg) {
		t.Fatal("different panel shares node journal")
	}
	cfg.Panel.URL = "https://panel-a.example/api"
	cfg.Panel.NodeID = 5
	if first == newTrafficSpool(cfg) {
		t.Fatal("sibling node shares node journal")
	}
}

func TestTrafficSpoolSplitsSumAtSignedBoundary(t *testing.T) {
	q := trafficSpool{dir: filepath.Join(t.TempDir(), "traffic-batches", "node")}
	if err := q.prepare(); err != nil {
		t.Fatal(err)
	}
	tr := tracker.New()
	tr.Process(map[int][2]int64{5: {math.MaxInt64, 1}, 6: {1, 1}}, nil, 0)
	if err := q.freeze(tr, 2); err != nil {
		t.Fatal(err)
	}
	names, _, err := q.list()
	if err != nil || len(names) != 2 {
		t.Fatal(names, err)
	}
}
