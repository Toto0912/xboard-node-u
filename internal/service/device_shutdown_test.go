package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/panel"
)

type recordingDevicePush struct {
	devices map[int][]string
}

func (*recordingDevicePush) Run(context.Context) {}
func (*recordingDevicePush) IsConnected() bool   { return true }
func (p *recordingDevicePush) SendDeviceReport(devices map[int][]string) {
	p.devices = devices
}

func TestStoppedKernelReportsEmptyDevicesAndPreservesRestartTraffic(t *testing.T) {
	for _, mode := range []string{"panel", "machine"} {
		t.Run(mode, func(t *testing.T) {
			type wireReport struct {
				NodeID    int              `json:"node_id"`
				MachineID int              `json:"machine_id"`
				Alive     map[int][]string `json:"alive"`
				Online    map[int]int      `json:"online"`
				Traffic   map[int][2]int64 `json:"traffic"`
			}
			reports := make(chan wireReport, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload wireReport
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				reports <- payload
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			cfg := config.PanelConfig{URL: server.URL, NodeID: 4, Token: "test"}
			push := &recordingDevicePush{}
			var cp controlplane.ControlPlane
			if mode == "machine" {
				cfg.MachineID = 7
				cp = controlplane.NewMachinePanelControlPlane(panel.NewClient(cfg), config.KernelConfig{}, push, nil)
			} else {
				cp = controlplane.NewPanelControlPlane(cfg, config.WSConfig{}, config.KernelConfig{})
			}
			k := &fakeKernel{running: true}
			s := newTestService(k)
			s.source, s.sink, s.wsClient = cp, cp, push
			s.lastConfig = &model.NodeSpec{Protocol: "vless"}
			users := []model.UserSpec{{ID: 2, UUID: "u2"}}
			s.updateUserState(users)
			s.tracker.Process(map[int][2]int64{2: {100, 200}}, map[int]map[string]bool{2: {"192.0.2.4": true}}, 1)
			s.reportDevices()
			if len(push.devices[2]) != 1 {
				t.Fatal("running kernel's device did not reach WebSocket transport")
			}
			if mode == "panel" {
				s.applyUserUpdate(context.Background(), nil, computeUserHash(nil))
			} else {
				k.running = false // A child kernel exited while the machine socket is still connected.
			}
			metrics := s.wsMetrics()
			if metrics["kernel_status"] != false || metrics["active_users"] != 0 || metrics["active_connections"] != 0 ||
				metrics["inbound_speed"] != int64(0) || metrics["outbound_speed"] != int64(0) {
				t.Fatalf("stopped heartbeat still reports activity: %v", metrics)
			}
			s.pushReportSync(5 * time.Second)
			if len(reports) == 0 {
				t.Fatal("missing HTTP report after stop")
			}
			r := <-reports
			if r.NodeID != cfg.NodeID || r.MachineID != cfg.MachineID || r.Alive == nil || len(r.Alive) != 0 ||
				len(r.Online) != 0 || r.Traffic[2] != [2]int64{100, 200} {
				t.Fatalf("stopped HTTP report = %+v", r)
			}
			s.reportDevices()
			if push.devices == nil || len(push.devices) != 0 {
				t.Fatalf("stopped WS devices = %#v, want explicit empty snapshot", push.devices)
			}
			if !s.startKernel(s.lastConfig, users) {
				t.Fatal("could not restart kernel")
			}
			s.tracker.Process(map[int][2]int64{2: {130, 250}}, map[int]map[string]bool{2: {"192.0.2.5": true}}, 1)
			s.pushReportSync(5 * time.Second)
			if len(reports) == 0 {
				t.Fatal("missing HTTP report after restart")
			}
			r = <-reports
			if r.Traffic[2] != [2]int64{30, 50} || len(r.Alive[2]) != 1 || r.Alive[2][0] != "192.0.2.5" {
				t.Fatalf("first report after restart = %+v", r)
			}
		})
	}
}

func TestUnexpectedStopClearsActivityWithoutAnHTTPReport(t *testing.T) {
	for _, tick := range []string{"ws", "tracking", "restart"} {
		t.Run(tick, func(t *testing.T) {
			s := newTestService(&fakeKernel{})
			s.tracker.Process(map[int][2]int64{1: {3, 4}}, map[int]map[string]bool{1: {"192.0.2.1": true}}, 1)
			switch tick {
			case "ws":
				push := &recordingDevicePush{}
				s.wsClient = push
				s.sink = controlplane.NewMachinePanelControlPlane(nil, config.KernelConfig{}, push, nil)
				s.reportDevices()
				if push.devices == nil || len(push.devices) != 0 {
					t.Fatalf("unexpected stop WS devices = %#v", push.devices)
				}
			case "tracking":
				s.trackAndEnforce(context.Background())
			case "restart":
				if !s.startKernel(&model.NodeSpec{Protocol: "vless"}, []model.UserSpec{{ID: 1, UUID: "u1"}}) {
					t.Fatal("failed to restart")
				}
			}
			if len(s.tracker.CurrentOnline()) != 0 || s.tracker.ActiveConnections() != 0 || !s.tracker.HasTraffic() {
				t.Fatal("stale activity or lost pending traffic")
			}
		})
	}
}

type blockedReportSink struct {
	controlplane.Sink
	reports      chan controlplane.ReportPayload
	releaseFirst chan struct{}
	first        bool
}

func (*blockedReportSink) SupportsReporting() bool { return true }
func (s *blockedReportSink) Report(p controlplane.ReportPayload) error {
	first := s.first
	s.first = false
	s.reports <- p
	if first {
		<-s.releaseFirst
		return errors.New("previous report failed")
	}
	return nil
}

func TestFinalReportWaitsForOldSnapshotAndRestoredTraffic(t *testing.T) {
	s := newTestService(&fakeKernel{running: true})
	sink := &blockedReportSink{reports: make(chan controlplane.ReportPayload, 2), releaseFirst: make(chan struct{}), first: true}
	s.source, s.sink = &scriptedPollingSource{}, sink
	s.tracker.Process(map[int][2]int64{1: {30, 40}}, map[int]map[string]bool{1: {"192.0.2.1": true}}, 1)
	s.pushReportAsync()
	select {
	case first := <-sink.reports:
		if len(first.Alive[1]) != 1 {
			t.Fatalf("first report = %+v", first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing initial report")
	}
	s.stopKernel()
	s.pushReportSync(20 * time.Millisecond)
	if len(sink.reports) != 0 {
		t.Fatal("final report overtook an outstanding request")
	}
	close(sink.releaseFirst)
	s.pushReportSync(5 * time.Second)
	if len(sink.reports) == 0 {
		t.Fatal("missing final report")
	}
	final := <-sink.reports
	if final.Alive == nil || len(final.Alive) != 0 || len(final.Online) != 0 || final.Traffic[1] != [2]int64{30, 40} {
		t.Fatalf("final report after failed in-flight request = %+v", final)
	}
}
