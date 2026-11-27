package service

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/model"
)

func TestMailboxConfigDrivesGrantRecovery(t *testing.T) {
	for _, mode := range []string{"combined", "invalid then unchanged", "overlap"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			k := &fakeKernel{running: true}
			s := newTestService(k)
			old := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
			next := &model.NodeSpec{Protocol: "vless", ServerPort: 9443}
			a := model.UserSpec{ID: 1, UUID: "a", SpeedLimit: 4, DeviceLimit: 4}
			b := model.UserSpec{ID: 2, UUID: "b"}
			c := model.UserSpec{ID: 3, UUID: "c"}
			s.lastConfig, s.lastConfigHash = old, computeConfigHash(old)
			s.updateUserState([]model.UserSpec{a, b})
			s.appliedState.Config, s.appliedState.Users = old, s.lastUsers
			s.machineMailbox = controlplane.NewNodeMailbox()
			s.machineMailbox.SeedBaseline(s.lastUsers, old)
			s.machineMailbox.MarkReady()
			started := make(chan int, 8)
			response := make(chan controlplane.Snapshot)
			src := &scriptedPollingSource{pollFn: func(call int) (controlplane.Snapshot, error) {
				started <- call
				select {
				case snap := <-response:
					return snap, nil
				case <-ctx.Done():
					return controlplane.Snapshot{}, ctx.Err()
				}
			}}
			s.source = src
			expectCall := func(id int) {
				t.Helper()
				select {
				case n := <-started:
					if n != id {
						t.Fatalf("call=%d want=%d", n, id)
					}
				case <-time.After(time.Second):
					t.Fatal("no request before periodic poll")
				}
			}
			respond := func(snap controlplane.Snapshot) {
				t.Helper()
				response <- snap
				select {
				case r := <-s.pullResults:
					s.applyPullResult(ctx, r)
				case <-time.After(time.Second):
					t.Fatal("no REST result")
				}
			}
			event := func(kind controlplane.EventType, cfg *model.NodeSpec, users []model.UserSpec) {
				s.machineMailbox.Apply(controlplane.Event{Type: kind, Config: cfg, Users: users})
			}
			commitCount := 0
			commit := func() { commitCount++ }
			strict := a
			strict.SpeedLimit, strict.DeviceLimit = 1, 1
			if mode == "invalid then unchanged" {
				event(controlplane.EventSyncConfig, &model.NodeSpec{Protocol: "invalid"}, nil)
				s.drainMachineMailbox(ctx)
				expectCall(1)
				respond(controlplane.Snapshot{Config: old, Users: nil, CommitUsers: commit})
				if !s.configPending || commitCount != 0 {
					t.Fatal("304 users confirmed the rejected configuration")
				}
				event(controlplane.EventSyncConfig, old, nil)
				s.drainMachineMailbox(ctx)
				expectCall(2)
				respond(controlplane.Snapshot{Config: old, Users: []model.UserSpec{}, CommitUsers: commit})
				if s.configPending || len(s.lastUsers) != 0 || k.IsRunning() || commitCount != 1 {
					t.Fatal("explicit empty list did not revoke every user")
				}
				return
			}
			if mode == "overlap" {
				s.pullViaAPIAsync(ctx)
				expectCall(1)
			}
			event(controlplane.EventSyncUsers, nil, []model.UserSpec{strict, c})
			event(controlplane.EventSyncConfig, next, nil)
			s.drainMachineMailbox(ctx)
			if !s.configPending || !reflect.DeepEqual(s.lastUsers, []model.UserSpec{strict}) {
				t.Fatalf("rights before confirmation: %+v", s.lastUsers)
			}
			if mode == "overlap" {
				event(controlplane.EventConfigPending, nil, nil)
				s.drainMachineMailbox(ctx)
				select {
				case <-started:
					t.Fatal("overlapping REST polls")
				default:
				}
				respond(controlplane.Snapshot{Config: old, Users: []model.UserSpec{a, b}, CommitUsers: commit})
				expectCall(2)
				if commitCount != 0 || !reflect.DeepEqual(s.lastUsers, []model.UserSpec{strict}) {
					t.Fatal("late response restored grants")
				}
			} else {
				expectCall(1)
			}
			respond(controlplane.Snapshot{Config: next, Users: []model.UserSpec{a, c}, CommitUsers: commit})
			if s.configPending || commitCount != 1 || !reflect.DeepEqual(s.lastUsers, []model.UserSpec{a, c}) {
				t.Fatal("missing confirmed recovery")
			}
			if s.hasPendingForceResync() || s.pullActive.Load() {
				t.Fatal("redundant reconciliation")
			}
			t.Logf("mode=%s: before REST=[1 limits 1/1]; after confirmation=[1 limits 4/4,3]; max simultaneous polls=1", mode)
		})
	}
}
