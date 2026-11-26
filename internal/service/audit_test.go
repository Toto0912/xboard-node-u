package service

import (
	"context"
	"errors"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/model"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTLSCandidateCanRecoverLocalSelfFromNone(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}, Cert: config.CertConfig{CertMode: "self", Domain: "self.test", CertDir: t.TempDir()}}
	s := newService(cfg, nil)
	defer s.cert.Stop()
	s.kernel = &fakeKernel{protocols: []string{"hysteria", "vless"}}
	ctx := context.Background()
	s.updateUserState([]model.UserSpec{})
	none := &model.NodeSpec{Protocol: "vless", ServerPort: 443, CertConfig: &config.CertConfig{CertMode: "none"}}
	s.applyPullResult(ctx, pullResult{config: none, configHash: computeConfigHash(none)})
	next := &model.NodeSpec{Protocol: "hysteria", Version: 2, ServerPort: 443}
	committed := false
	s.applyPullResult(ctx, pullResult{config: next, configHash: computeConfigHash(next), commitConfig: func() { committed = true }})
	if !committed || !s.cert.HasCert() {
		t.Fatal("restored certificate rejected")
	}
}
func TestJournalListConcurrentRenames(t *testing.T) {
	dir := t.TempDir()
	q := trafficSpool{dir: dir}
	done := make(chan error, 1)
	go func() {
		for range 100 {
			if err := q.put([][3]int64{{1, 2, 3}}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		if _, _, err := q.list(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			names, _, e := q.list()
			if e != nil || len(names) != 100 {
				t.Fatalf("%d %v", len(names), e)
			}
			if err := os.WriteFile(filepath.Join(dir, "unknown"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, e := q.list(); e == nil {
				t.Fatal("unknown file ignored")
			}
			return
		default:
		}
	}
}

func TestRejectedConfigAppliesOnlyAuthenticatedRestrictions(t *testing.T) {
	ctx := context.Background()
	k := &fakeKernel{}
	s := newTestService(k)
	defer s.cert.Stop()
	good := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	old := []model.UserSpec{{ID: 1, UUID: "expired"}, {ID: 2, UUID: "retained", SpeedLimit: 5, DeviceLimit: 4}}
	desired := []model.UserSpec{{ID: 2, UUID: "retained", SpeedLimit: 0, DeviceLimit: 1}, {ID: 3, UUID: "new", SpeedLimit: 2}}
	s.applyPullResult(ctx, pullResult{config: good, configHash: computeConfigHash(good), users: old, userHash: computeUserHash(old)})
	mb := controlplane.NewNodeMailbox()
	mb.SeedBaseline(old, good)
	mb.MarkReady()
	s.machineMailbox = mb
	committed := false
	s.source = &scriptedPollingSource{pollFn: func(int) (controlplane.Snapshot, error) {
		return controlplane.Snapshot{Users: desired}, errors.New("candidate config rejected")
	}}
	s.pullViaAPIWithOptions(ctx, false)
	select {
	case r := <-s.pullResults:
		r.commitConfig = func() { t.Fatal("rejected config ETag") }
		r.commitUsers = func() { t.Fatal("intersected list ETag") }
		s.applyPullResult(ctx, r)
	case <-time.After(time.Second):
		t.Fatal("partial pull lost")
	}
	want := []model.UserSpec{{ID: 2, UUID: "retained", SpeedLimit: 5, DeviceLimit: 1}}
	if !reflect.DeepEqual(s.appliedState.Users, want) || !reflect.DeepEqual(s.lastUsers, want) || s.lastConfig != good || !k.IsRunning() {
		t.Fatalf("old config restricted users: %+v %+v", s.appliedState.Users, s.lastUsers)
	}
	next := &model.NodeSpec{Protocol: "vless", ServerPort: 444}
	s.applyPullResult(ctx, pullResult{config: next, configHash: computeConfigHash(next), users: desired, userHash: computeUserHash(desired), commitUsers: func() { committed = true }})
	if !reflect.DeepEqual(s.appliedState.Users, desired) || !committed {
		t.Fatalf("valid follow-up did not converge: %+v", s.appliedState.Users)
	}
}

func TestCoalescedWSStatusSupersedesInflightForce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newTestService(&fakeKernel{})
	defer s.cert.Stop()
	s.wsStatus = controlplane.NewStatusMailbox()
	good := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	users := []model.UserSpec{{ID: 1, UUID: "kept"}, {ID: 2, UUID: "revoked"}}
	s.applyPullResult(ctx, pullResult{config: good, configHash: computeConfigHash(good), users: users, userHash: computeUserHash(users)})
	started := make(chan struct{})
	release := make(chan struct{})
	var applied bool
	p := &scriptedPollingSource{pollFn: func(call int) (controlplane.Snapshot, error) {
		if call == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return controlplane.Snapshot{}, ctx.Err()
			}
			return controlplane.Snapshot{Users: users, CommitUsers: func() { t.Fatal("committed stale user ETag") }}, nil
		}
		return controlplane.Snapshot{Users: users[:1], CommitUsers: func() { applied = true }}, nil
	}}
	s.source = p
	s.pullViaAPIWithOptions(ctx, true)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no first pull")
	}
	for range 20 {
		s.wsStatus.Publish(controlplane.StatusChange{Connected: true, NeedsResync: true})
		s.wsStatus.Publish(controlplane.StatusChange{Connected: false})
	}
	s.wsStatus.Publish(controlplane.StatusChange{Connected: true})
	close(release)
	// Deliberately handle the old REST result before reading the pending status wakeup.
	next := func() pullResult {
		t.Helper()
		select {
		case r := <-s.pullResults:
			return r
		case <-time.After(time.Second):
			t.Fatal("missing reconciliation")
			return pullResult{}
		}
	}
	s.applyPullResult(ctx, next())
	if applied {
		t.Fatal("expected fresh pull")
	}
	s.applyPullResult(ctx, next())
	if !applied || s.hasPendingForceResync() || !reflect.DeepEqual(s.appliedState.Users, users[:1]) || !s.wsDisconnectAt.IsZero() {
		t.Fatalf("did not converge: %+v", s.appliedState.Users)
	}
	select {
	case <-s.wsStatus.NotifyCh():
		s.drainWSStatus(ctx)
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pollCalls != 2 || p.resetUserCalls != 2 {
		t.Fatalf("unexpected polling: polls=%d resets=%d", p.pollCalls, p.resetUserCalls)
	}
}
