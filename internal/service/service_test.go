package service

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/cedar2025/xboard-node/internal/cert"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/limiter"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/porthopping"
	"github.com/cedar2025/xboard-node/internal/tracker"
	"golang.org/x/time/rate"
)

type fakeKernel struct {
	running bool

	startErr  error
	reloadErr error
	updateErr error
	addErr    error
	removeErr error

	startCalls  int
	stopCalls   int
	reloadCalls int
	updateCalls int
	addCalls    int
	removeCalls int

	startHistory  []kernel.TLSCert
	reloadHistory []kernel.TLSCert

	onReload      func([]model.UserSpec)
	onStart       func(*model.NodeSpec, []model.UserSpec, kernel.TLSCert)
	onUpdateUsers func([]model.UserSpec)
	onAddUsers    func([]model.UserSpec)
	onRemoveUsers func([]model.UserSpec)

	speedLimitFunc  func(string) *rate.Limiter
	deviceLimitFunc func(string) (int, bool)

	protocols []string
}

func (f *fakeKernel) Name() string { return "fake" }
func (f *fakeKernel) Protocols() []string {
	if len(f.protocols) > 0 {
		return f.protocols
	}
	return []string{"vless"}
}
func (f *fakeKernel) Capabilities() kernel.Capabilities { return kernel.Capabilities{} }
func (f *fakeKernel) Start(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	f.startCalls++
	f.startHistory = append(f.startHistory, kernel.TLSCert{
		CertPEM: append([]byte(nil), tls.CertPEM...),
		KeyPEM:  append([]byte(nil), tls.KeyPEM...),
	})
	if f.onStart != nil {
		f.onStart(nodeConfig, users, tls)
	}
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	return nil
}
func (f *fakeKernel) Stop() {
	f.stopCalls++
	f.running = false
}
func (f *fakeKernel) IsRunning() bool { return f.running }
func (f *fakeKernel) Reload(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	_, _, _ = nodeConfig, users, tls
	f.reloadCalls++
	if f.onReload != nil {
		f.onReload(users)
	}
	f.reloadHistory = append(f.reloadHistory, kernel.TLSCert{
		CertPEM: append([]byte(nil), tls.CertPEM...),
		KeyPEM:  append([]byte(nil), tls.KeyPEM...),
	})
	if f.reloadErr != nil {
		return f.reloadErr
	}
	f.running = true
	return nil
}
func (f *fakeKernel) AddUsers(users []model.UserSpec) (int, error) {
	f.addCalls++
	if f.onAddUsers != nil {
		f.onAddUsers(users)
	}
	if f.addErr != nil {
		return 0, f.addErr
	}
	return len(users), nil
}
func (f *fakeKernel) RemoveUsers(users []model.UserSpec) (int, error) {
	f.removeCalls++
	if f.onRemoveUsers != nil {
		f.onRemoveUsers(users)
	}
	if f.removeErr != nil {
		return 0, f.removeErr
	}
	return len(users), nil
}
func (f *fakeKernel) UpdateUsers(users []model.UserSpec) (int, int, error) {
	f.updateCalls++
	if f.onUpdateUsers != nil {
		f.onUpdateUsers(users)
	}
	if f.updateErr != nil {
		return 0, 0, f.updateErr
	}
	return len(users), 0, nil
}
func (f *fakeKernel) GetUserTraffic(ctx context.Context) (map[int][2]int64, map[int]map[string]bool, int, error) {
	_ = ctx
	return nil, nil, 0, nil
}
func (f *fakeKernel) CloseConnection(ctx context.Context, connID string) error {
	_, _ = ctx, connID
	return nil
}
func (f *fakeKernel) CloseUserConnections(ctx context.Context, uuid string) error {
	_, _ = ctx, uuid
	return nil
}
func (f *fakeKernel) SetSpeedLimitFunc(fn func(uuid string) *rate.Limiter) { f.speedLimitFunc = fn }
func (f *fakeKernel) SetDeviceLimitFunc(fn func(uuid string) (int, bool))  { f.deviceLimitFunc = fn }
func (f *fakeKernel) UpdateGlobalDevices(users map[int][]string)           { _ = users }
func (f *fakeKernel) ClearGlobalDevices()                                  {}

func newTestService(k *fakeKernel) *Service {
	sharedLimiter := limiter.New()
	s := &Service{
		cfg:          &config.Config{},
		kernel:       k,
		tracker:      tracker.New(),
		limiter:      sharedLimiter,
		speedTracker: limiter.NewSpeedTracker(sharedLimiter),
		cert:         cert.NewManager(config.CertConfig{}),
		pullResults:  make(chan pullResult, 1),
	}
	k.SetSpeedLimitFunc(s.speedTracker.GetLimiter)
	k.SetDeviceLimitFunc(s.limiter.GetDeviceLimitByUUID)
	return s
}

func TestApplyUserUpdatePreparesLimiterBeforeKernelUpdate(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	newUsers := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	k.onUpdateUsers = func(users []model.UserSpec) {
		if len(users) != 1 || users[0].UUID != "uuid-new" {
			t.Fatalf("unexpected users passed to UpdateUsers: %#v", users)
		}
		if got := k.speedLimitFunc("uuid-new"); got == nil {
			t.Fatal("expected new user's limiter to be visible before kernel UpdateUsers")
		}
	}

	s.applyUserUpdate(context.Background(), newUsers, computeUserHash(newUsers))

	if got := k.updateCalls; got != 1 {
		t.Fatalf("UpdateUsers call count = %d, want 1", got)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-new" {
		t.Fatalf("lastUsers = %#v, want new users", s.lastUsers)
	}
	if s.speedTracker.GetLimiter("uuid-new") == nil {
		t.Fatal("expected limiter for new user after successful update")
	}
}

func TestApplyUserUpdateKeepsRevocationsWhenKernelAndRestartFail(t *testing.T) {
	k := &fakeKernel{
		running:   true,
		updateErr: errors.New("update failed"),
		startErr:  errors.New("restart failed"),
	}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	newUsers := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	s.applyUserUpdate(context.Background(), newUsers, computeUserHash(newUsers))

	if got := k.startCalls; got != 1 {
		t.Fatalf("Start call count = %d, want 1", got)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-new" || !s.usersNeedsReapply || k.IsRunning() {
		t.Fatalf("failed rebuild left an unsafe target or running kernel: %#v", s.lastUsers)
	}
	if s.speedTracker.GetLimiter("uuid-old") != nil || s.speedTracker.GetLimiter("uuid-new") == nil {
		t.Fatal("limiter must track the safe target")
	}
}

func TestApplyUserDeltaAddPreparesLimiterBeforeKernelUpdate(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	delta := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	k.onAddUsers = func(users []model.UserSpec) {
		if len(users) != 1 || users[0].UUID != "uuid-new" {
			t.Fatalf("unexpected users passed to AddUsers: %#v", users)
		}
		if got := k.speedLimitFunc("uuid-new"); got == nil {
			t.Fatal("expected delta user's limiter to be visible before kernel AddUsers")
		}
	}

	s.applyUserDelta(context.Background(), "add", delta)

	if got := k.addCalls; got != 1 {
		t.Fatalf("AddUsers call count = %d, want 1", got)
	}
	if s.speedTracker.GetLimiter("uuid-new") == nil {
		t.Fatal("expected limiter for delta-added user after successful update")
	}
}

func TestValidateNodeRuntimeRejectsUnsupportedDNSProvider(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"http"}, &model.NodeSpec{
		Protocol: "http",
		CertConfig: &config.CertConfig{
			CertMode:    "dns",
			DNSProvider: "3123123",
			Domain:      "example.com",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() == "" {
		t.Fatal("expected non-empty error")
	}
	if got := err.Error(); !strings.HasPrefix(got, `unsupported cert_config.dns_provider "3123123" (supported: `) {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestValidateNodeRuntimeAllowsSelfManagedTLSBeforeFilesExist(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"anytls", "hysteria"}, &model.NodeSpec{
		Protocol: "anytls",
		CertConfig: &config.CertConfig{
			CertMode: "self",
			Domain:   "example.com",
		},
	}, kernel.TLSCert{})
	if err != nil {
		t.Fatalf("expected self-managed TLS config to pass validation, got %v", err)
	}
}

func TestValidateNodeRuntimeAllowsSingboxRealityWithRequiredFields(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
		TLSSettings: map[string]any{
			"private_key": "test-key",
			"server_name": "example.com",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err != nil {
		t.Fatalf("expected sing-box reality validation to pass, got %v", err)
	}
}

func TestValidateNodeRuntimeRejectsRealityWithoutTLSSettings(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "reality tls requires tls_settings" {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestValidateNodeRuntimeRejectsRealityWithoutPrivateKey(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
		TLSSettings: map[string]any{
			"server_name": "example.com",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "reality tls requires tls_settings.private_key" {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestValidateNodeRuntimeRejectsRealityWithoutServerNameOrDest(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
		TLSSettings: map[string]any{
			"private_key": "test-key",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "reality tls requires tls_settings.server_name or tls_settings.dest" {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestApplyUserUpdateAndDelta_ZeroToOneToZeroAndAppliedStateSync(t *testing.T) {
	k := &fakeKernel{running: false}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s.updateUserState([]model.UserSpec{})

	// 0 -> 1 via applyUserUpdate must start the kernel.
	u1 := []model.UserSpec{{ID: 1, UUID: "uuid-1"}}
	s.applyUserUpdate(context.Background(), u1, computeUserHash(u1))
	if !k.IsRunning() || k.startCalls != 1 {
		t.Fatalf("expected 0->1 applyUserUpdate to start kernel, running=%v startCalls=%d", k.IsRunning(), k.startCalls)
	}
	if len(s.appliedState.Users) != 1 || s.appliedState.Users[0].ID != 1 {
		t.Fatalf("expected appliedState.Users=[1], got %#v", s.appliedState.Users)
	}

	// Delta add user 2 -> appliedState.Users must include both 1 and 2.
	s.applyUserDelta(context.Background(), "add", []model.UserSpec{{ID: 2, UUID: "uuid-2"}})
	if len(s.appliedState.Users) != 2 {
		t.Fatalf("expected appliedState.Users to track 2 users after delta add, got %#v", s.appliedState.Users)
	}

	// Delta remove user 1 -> appliedState.Users must drop user 1 so rollback will not resurrect it.
	s.applyUserDelta(context.Background(), "remove", []model.UserSpec{{ID: 1}})
	if len(s.appliedState.Users) != 1 || s.appliedState.Users[0].ID != 2 {
		t.Fatalf("expected appliedState.Users=[2] after delta remove, got %#v", s.appliedState.Users)
	}

	// 1 -> 0 via applyUserUpdate must stop the kernel and clear appliedState.Users.
	s.applyUserUpdate(context.Background(), []model.UserSpec{}, computeUserHash([]model.UserSpec{}))
	if k.IsRunning() || k.stopCalls != 1 || len(s.appliedState.Users) != 0 {
		t.Fatalf("expected 1->0 to stop kernel & clear appliedState, running=%v stops=%d applied=%#v", k.IsRunning(), k.stopCalls, s.appliedState.Users)
	}

	// 0 -> 1 via delta add must start kernel again.
	s.applyUserDelta(context.Background(), "add", []model.UserSpec{{ID: 3, UUID: "uuid-3"}})
	if !k.IsRunning() || k.startCalls != 2 || len(s.appliedState.Users) != 1 || s.appliedState.Users[0].ID != 3 {
		t.Fatalf("expected 0->1 delta add to restart kernel, running=%v startCalls=%d applied=%#v", k.IsRunning(), k.startCalls, s.appliedState.Users)
	}

	// Removing last user via delta must stop kernel cleanly.
	s.applyUserDelta(context.Background(), "remove", []model.UserSpec{{ID: 3}})
	if k.IsRunning() || k.stopCalls != 2 || len(s.appliedState.Users) != 0 {
		t.Fatalf("expected delta remove of last user to stop kernel, running=%v stops=%d applied=%#v", k.IsRunning(), k.stopCalls, s.appliedState.Users)
	}
}

func TestHandleCertRenewal_ReloadsKernelAndRetriesAfterStoppedFailure(t *testing.T) {
	k := &fakeKernel{
		running:   true,
		reloadErr: errors.New("reload err"),
		startErr:  errors.New("start err"),
	}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s.lastUsers = []model.UserSpec{{ID: 1, UUID: "uuid-1"}}

	// Trigger real cert renewal notification.
	s.cert.MarkRenewed()
	s.handleCertRenewal(context.Background())

	// Both reload and restart fallback failed: pending renewal MUST remain true even if kernel stopped.
	if k.reloadCalls != 1 || k.startCalls != 1 {
		t.Fatalf("expected reloadCalls=1 startCalls=1, got reload=%d start=%d", k.reloadCalls, k.startCalls)
	}
	if !s.cert.HasPendingRenewal() {
		t.Fatal("expected HasPendingRenewal() to remain true after failed reload")
	}

	// Simulate transient issue resolved while kernel is stopped (!k.IsRunning()).
	k.running = false
	k.reloadErr = nil
	k.startErr = nil

	s.handleCertRenewal(context.Background())
	if !k.IsRunning() || k.startCalls != 2 {
		t.Fatalf("expected retry when stopped to call startKernel and recover running state, running=%v startCalls=%d", k.IsRunning(), k.startCalls)
	}
	if s.cert.HasPendingRenewal() {
		t.Fatal("expected HasPendingRenewal() to clear after successful recovery start")
	}
}

func TestApplyUserDelta_SingleUserUUIDChangeUsesUpdateWithoutStop(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "uuid-old"}})

	// Make RemoveUsers simulate Xray's stop-on-last-user behavior to prove it is not called.
	k.onRemoveUsers = func(users []model.UserSpec) {
		k.Stop()
	}

	s.applyUserDelta(context.Background(), "add", []model.UserSpec{{ID: 1, UUID: "uuid-new"}})

	if k.removeCalls != 0 {
		t.Fatalf("expected no RemoveUsers call on UUID change, got %d", k.removeCalls)
	}
	if k.stopCalls != 0 || !k.IsRunning() {
		t.Fatalf("expected kernel to stay running without stopping, stopCalls=%d running=%v", k.stopCalls, k.IsRunning())
	}
	if k.updateCalls != 1 {
		t.Fatalf("expected UpdateUsers to be called once, got %d", k.updateCalls)
	}
	if len(s.appliedState.Users) != 1 || s.appliedState.Users[0].UUID != "uuid-new" {
		t.Fatalf("expected appliedState.Users to have uuid-new, got %#v", s.appliedState.Users)
	}
}

func TestApplyPullResultRollbackRestoresConfigHashAndSkipsCommit(t *testing.T) {
	k := &fakeKernel{
		running:   true,
		reloadErr: errors.New("reload failed"),
		startErr:  errors.New("address already in use"),
		updateErr: errors.New("update failed"),
	}
	s := newTestService(k)
	oldCfg := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	oldHash := computeConfigHash(oldCfg)
	s.lastConfig = oldCfg
	s.lastConfigHash = oldHash
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "uuid-1"}})

	newCfg := &model.NodeSpec{Protocol: "vless", ServerPort: 8443}
	newHash := computeConfigHash(newCfg)

	cfgCommitted := false
	usersCommitted := false
	s.applyPullResult(context.Background(), pullResult{
		config:       newCfg,
		configHash:   newHash,
		users:        []model.UserSpec{{ID: 2, UUID: "uuid-2"}},
		userHash:     computeUserHash([]model.UserSpec{{ID: 2, UUID: "uuid-2"}}),
		commitConfig: func() { cfgCommitted = true },
		commitUsers:  func() { usersCommitted = true },
	})

	if cfgCommitted || usersCommitted {
		t.Fatalf("expected ETags not to be committed on kernel apply failure, cfg=%v users=%v", cfgCommitted, usersCommitted)
	}
	if s.lastConfigHash != oldHash || s.lastConfig.ServerPort != 443 {
		t.Fatalf("expected config and hash rollback to 443/%s, got %d/%s", oldHash, s.lastConfig.ServerPort, s.lastConfigHash)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != 2 || !s.usersNeedsReapply || k.IsRunning() || len(s.safeUsersForRestart()) != 0 {
		t.Fatalf("failed combined update must retain unconfirmed ID=2 with no authorized rollback users: %#v", s.lastUsers)
	}
}

func TestDrainMachineMailbox_RemovesLastUserStopsKernel(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.cfg = &config.Config{}
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	u := []model.UserSpec{{ID: 1, UUID: "uuid-1"}}
	s.updateUserState(u)
	s.appliedState.Users = append([]model.UserSpec(nil), u...)

	mb := controlplane.NewNodeMailbox()
	mb.SeedBaseline(u, s.lastConfig)
	mb.MarkReady()
	_ = mb.DrainIfReady()
	s.machineMailbox = mb

	mb.Apply(controlplane.Event{
		Type:        controlplane.EventSyncUserDelta,
		DeltaAction: "remove",
		DeltaUsers:  []model.UserSpec{{ID: 1}},
	})

	s.drainMachineMailbox(context.Background())

	if k.IsRunning() || k.stopCalls != 1 {
		t.Fatalf("expected removing last user via machine mailbox to stop kernel, running=%v stops=%d", k.IsRunning(), k.stopCalls)
	}
	if len(s.appliedState.Users) != 0 {
		t.Fatalf("expected empty appliedState.Users, got %#v", s.appliedState.Users)
	}
}

type zeroUserCertControlPlane struct {
	bootstrap controlplane.Bootstrap
}

func (z *zeroUserCertControlPlane) Initial(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.Bootstrap, error) {
	return z.bootstrap, nil
}
func (z *zeroUserCertControlPlane) Poll(context.Context) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, nil
}
func (z *zeroUserCertControlPlane) Discover(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.PushClient, error) {
	return nil, nil
}
func (z *zeroUserCertControlPlane) Metrics() controlplane.APIMetrics {
	return controlplane.APIMetrics{}
}
func (z *zeroUserCertControlPlane) SupportsPolling() bool   { return false }
func (z *zeroUserCertControlPlane) SupportsDiscovery() bool { return false }

func TestInitialSetup_ZeroUsersAppliesRemoteCertOverrideBeforeFirstUser(t *testing.T) {
	k := &fakeKernel{}
	s := newTestService(k)
	s.cfg = &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	s.source = &zeroUserCertControlPlane{
		bootstrap: controlplane.Bootstrap{
			Config: &model.NodeSpec{
				Protocol:   "vless",
				ServerPort: 443,
				TLS:        1,
				CertConfig: &config.CertConfig{
					CertMode: "self",
					Domain:   "zero-start.example.com",
				},
			},
			Users: []model.UserSpec{},
		},
	}

	if err := s.initialSetup(context.Background()); err != nil {
		t.Fatalf("initialSetup failed: %v", err)
	}
	if k.IsRunning() {
		t.Fatal("expected kernel not to start with 0 users")
	}
	if !s.cert.HasCert() {
		t.Fatal("expected zero-user initialSetup to apply CertConfig override before first user arrives")
	}
}

type scriptedPollingSource struct {
	mu               sync.Mutex
	pollFn           func(call int) (controlplane.Snapshot, error)
	pollCalls        int
	resetConfigCalls int
	resetUserCalls   int
}

func (p *scriptedPollingSource) Initial(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.Bootstrap, error) {
	return controlplane.Bootstrap{}, nil
}
func (p *scriptedPollingSource) Poll(context.Context) (controlplane.Snapshot, error) {
	p.mu.Lock()
	p.pollCalls++
	call := p.pollCalls
	fn := p.pollFn
	p.mu.Unlock()
	return fn(call)
}
func (p *scriptedPollingSource) Discover(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.PushClient, error) {
	return nil, nil
}
func (p *scriptedPollingSource) Metrics() controlplane.APIMetrics { return controlplane.APIMetrics{} }
func (p *scriptedPollingSource) SupportsPolling() bool            { return true }
func (p *scriptedPollingSource) SupportsDiscovery() bool          { return false }
func (p *scriptedPollingSource) ResetConfigETag() {
	p.mu.Lock()
	p.resetConfigCalls++
	p.mu.Unlock()
}
func (p *scriptedPollingSource) ResetUserETag() {
	p.mu.Lock()
	p.resetUserCalls++
	p.mu.Unlock()
}

func generateTestPEM(t *testing.T, cn string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestLateRESTResponseDoesNotOverwriteAppliedWSRemoval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}

	u1 := model.UserSpec{ID: 1, UUID: "uuid-1"}
	u2 := model.UserSpec{ID: 2, UUID: "uuid-2"}
	u3 := model.UserSpec{ID: 3, UUID: "uuid-3"}
	initialUsers := []model.UserSpec{u1, u2}
	s.updateUserState(initialUsers)
	s.appliedState.Config = s.lastConfig
	s.appliedState.Users = cloneUsersNonNil(initialUsers)

	mb := controlplane.NewNodeMailbox()
	mb.SeedBaseline(initialUsers, s.lastConfig)
	mb.MarkReady()
	_ = mb.DrainIfReady()
	s.machineMailbox = mb

	poll1Entered := make(chan struct{})
	poll1Release := make(chan struct{})
	var commit1Called, commit2Called atomic.Bool

	src := &scriptedPollingSource{
		pollFn: func(call int) (controlplane.Snapshot, error) {
			if call == 1 {
				close(poll1Entered)
				<-poll1Release
				return controlplane.Snapshot{
					Users:       []model.UserSpec{u1, u2},
					CommitUsers: func() { commit1Called.Store(true) },
				}, nil
			}
			return controlplane.Snapshot{
				Users:       []model.UserSpec{u1},
				CommitUsers: func() { commit2Called.Store(true) },
			}, nil
		},
	}
	s.source = src

	// Start in-flight REST pull that captures [u1, u2].
	s.pullViaAPIWithOptions(ctx, false)
	<-poll1Entered

	// While REST is in flight, WS removes user 2 and main loop drains mailbox (dirtyUsers -> false).
	mb.Apply(controlplane.Event{
		Type:        controlplane.EventSyncUserDelta,
		DeltaAction: "remove",
		DeltaUsers:  []model.UserSpec{{ID: 2}},
	})
	s.drainMachineMailbox(ctx)

	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != 1 {
		t.Fatalf("after WS removal, lastUsers = %#v, want [1]", s.lastUsers)
	}

	// Release stale REST poll #1 and apply it.
	close(poll1Release)
	res1 := <-s.pullResults
	s.applyPullResult(ctx, res1)

	if commit1Called.Load() {
		t.Fatal("stale REST poll must NOT commit its UserETag")
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != 1 {
		t.Fatalf("stale REST poll resurrected revoked user: lastUsers = %#v", s.lastUsers)
	}
	if len(s.appliedState.Users) != 1 || s.appliedState.Users[0].ID != 1 {
		t.Fatalf("stale REST poll mutated appliedState.Users = %#v", s.appliedState.Users)
	}

	// Follow-up forced reconciliation poll #2 should already have been launched automatically.
	select {
	case res2 := <-s.pullResults:
		s.applyPullResult(ctx, res2)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for automatic follow-up forced reconciliation poll")
	}

	if !commit2Called.Load() {
		t.Fatal("expected clean forced poll #2 to commit UserETag")
	}
	src.mu.Lock()
	resetCfgCalls := src.resetConfigCalls
	resetUsrCalls := src.resetUserCalls
	src.mu.Unlock()
	if resetCfgCalls == 0 || resetUsrCalls == 0 {
		t.Fatalf("expected forced resync to reset both ETags, got config=%d user=%d", resetCfgCalls, resetUsrCalls)
	}

	// Prove mailbox baseline was NOT polluted with stale [u1, u2]: next WS delta add u3 yields [u1, u3].
	mb.Apply(controlplane.Event{
		Type:        controlplane.EventSyncUserDelta,
		DeltaAction: "add",
		DeltaUsers:  []model.UserSpec{u3},
	})
	s.drainMachineMailbox(ctx)
	if len(s.lastUsers) != 2 {
		t.Fatalf("expected [1, 3] after delta add, got %#v", s.lastUsers)
	}
	for _, u := range s.lastUsers {
		if u.ID == 2 {
			t.Fatalf("mailbox baseline leaked stale revoked user 2: %#v", s.lastUsers)
		}
	}
}

func TestCertFileRecoveryRetriesSameConfigWithout304(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	cert0Path := filepath.Join(tmpDir, "c0.crt")
	key0Path := filepath.Join(tmpDir, "c0.key")
	cert0PEM, key0PEM := generateTestPEM(t, "c0.example.com")
	if err := os.WriteFile(cert0Path, cert0PEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key0Path, key0PEM, 0o600); err != nil {
		t.Fatal(err)
	}

	cert1Path := filepath.Join(tmpDir, "c1.crt")
	key1Path := filepath.Join(tmpDir, "c1.key")
	cert1PEM, key1PEM := generateTestPEM(t, "c1.example.com")

	var mu sync.Mutex
	currentETag := `"etag-c0"`
	currentCertPath := cert0Path
	currentKeyPath := key0Path
	serverPort := 443
	var seenConfigIfNoneMatch []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		etag := currentETag
		cPath := currentCertPath
		kPath := currentKeyPath
		port := serverPort
		mu.Unlock()

		switch r.URL.Path {
		case "/api/v2/server/handshake":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"websocket": map[string]any{"enabled": false},
				"settings":  map[string]any{"push_interval": 60, "pull_interval": 60},
			})
		case "/api/v1/server/UniProxy/config":
			inm := r.Header.Get("If-None-Match")
			mu.Lock()
			seenConfigIfNoneMatch = append(seenConfigIfNoneMatch, inm)
			mu.Unlock()
			if inm == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"protocol":    "vless",
				"server_port": port,
				"tls":         1,
				"cert_config": map[string]any{
					"cert_mode": "file",
					"cert_file": cPath,
					"key_file":  kPath,
				},
			})
		case "/api/v1/server/UniProxy/user":
			uETag := `"etag-users"`
			if r.Header.Get("If-None-Match") == uETag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", uETag)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"users": []map[string]any{
					{"id": 1, "uuid": "11111111-1111-1111-1111-111111111111"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	k := &fakeKernel{}
	cfg := &config.Config{
		Panel: config.PanelConfig{URL: srv.URL, Token: "tok", NodeID: 1},
		Kernel: config.KernelConfig{
			Type: "singbox", // unused because fakeKernel overrides Protocols()
		},
	}
	cp := controlplane.NewPanelControlPlane(cfg.Panel, cfg.WS, cfg.Kernel)
	s := newService(cfg, cp)
	s.kernel = k
	s.kernel.SetSpeedLimitFunc(s.speedTracker.GetLimiter)
	s.kernel.SetDeviceLimitFunc(s.limiter.GetDeviceLimitByUUID)

	if err := s.initialSetup(ctx); err != nil {
		t.Fatalf("initialSetup failed: %v", err)
	}
	if !bytes.Equal(s.cert.TLSCert().CertPEM, cert0PEM) {
		t.Fatal("expected initial cert0 to be active")
	}
	if k.startCalls != 1 {
		t.Fatalf("startCalls = %d, want 1", k.startCalls)
	}

	// Switch panel to C1 pointing to cert1Path/key1Path before those files exist on disk.
	mu.Lock()
	currentETag = `"etag-c1"`
	currentCertPath = cert1Path
	currentKeyPath = key1Path
	serverPort = 8443
	mu.Unlock()

	s.pullViaAPIAsync(ctx)
	res1 := <-s.pullResults
	s.applyPullResult(ctx, res1)

	if s.lastConfig.ServerPort != 443 {
		t.Fatalf("lastConfig.ServerPort = %d, want 443 after cert failure", s.lastConfig.ServerPort)
	}
	if !bytes.Equal(s.cert.TLSCert().CertPEM, cert0PEM) {
		t.Fatal("expected cert0 to remain active in cert manager after failed Reconfigure")
	}
	if k.reloadCalls != 0 || k.startCalls != 1 {
		t.Fatalf("expected no kernel reload/restart on failed cert override, reload=%d start=%d", k.reloadCalls, k.startCalls)
	}

	// Write missing cert1/key1 files to disk WITHOUT changing server config or ETag.
	if err := os.WriteFile(cert1Path, cert1PEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key1Path, key1PEM, 0o600); err != nil {
		t.Fatal(err)
	}

	s.pullViaAPIAsync(ctx)
	res2 := <-s.pullResults
	s.applyPullResult(ctx, res2)

	if s.lastConfig.ServerPort != 8443 {
		t.Fatalf("expected recovery poll to apply C1 port 8443, got %d", s.lastConfig.ServerPort)
	}
	if !bytes.Equal(s.cert.TLSCert().CertPEM, cert1PEM) {
		t.Fatal("expected cert1 to be active after cert files recovered")
	}
	if !bytes.Equal(s.appliedState.TLSCert.CertPEM, cert1PEM) {
		t.Fatal("expected appliedState.TLSCert to record cert1")
	}
	if k.reloadCalls != 1 {
		t.Fatalf("expected 1 kernel reload after cert recovery, got %d", k.reloadCalls)
	}

	// Third poll must now send etag-c1 and receive 304.
	s.pullViaAPIAsync(ctx)
	res3 := <-s.pullResults
	s.applyPullResult(ctx, res3)

	mu.Lock()
	headers := append([]string(nil), seenConfigIfNoneMatch...)
	mu.Unlock()
	// Call 0: Initial(""), Call 1: Poll #1("etag-c0"), Call 2: Poll #2("etag-c0"), Call 3: Poll #3("etag-c1")
	if len(headers) != 4 {
		t.Fatalf("expected 4 config requests, got %v", headers)
	}
	if headers[1] != `"etag-c0"` || headers[2] != `"etag-c0"` || headers[3] != `"etag-c1"` {
		t.Fatalf("unexpected If-None-Match progression: %v (wanted [\"\" \"etag-c0\" \"etag-c0\" \"etag-c1\"])", headers)
	}
}

func TestApplyKernelRollbackRestoresPreviousAppliedTLSCert(t *testing.T) {
	t.Setenv("XBOARD_NODE_PORT_HOPPING_STATE_DIR", t.TempDir())
	ctx := context.Background()

	certA, keyA := generateTestPEM(t, "a.example.com")
	certB, keyB := generateTestPEM(t, "b.example.com")

	k := &fakeKernel{}
	s := newTestService(k)
	s.portHopping = porthopping.New(t.TempDir())

	cfgA := &model.NodeSpec{
		Protocol:   "vless",
		ServerPort: 443,
		CertConfig: &config.CertConfig{
			CertMode:    "content",
			CertContent: string(certA),
			KeyContent:  string(keyA),
		},
	}
	cfgAHash := computeConfigHash(cfgA)
	s.applyPullResult(ctx, pullResult{
		config:     cfgA,
		configHash: cfgAHash,
		users:      []model.UserSpec{{ID: 1, UUID: "uuid-1"}},
		userHash:   computeUserHash([]model.UserSpec{{ID: 1, UUID: "uuid-1"}}),
	})
	if !bytes.Equal(s.appliedState.TLSCert.CertPEM, certA) {
		t.Fatal("expected initial appliedState.TLSCert to equal certA")
	}

	// Next config switches to certB, but Reload & Start fail during rollout.
	k.reloadErr = errors.New("reload err")
	k.startErr = errors.New("bind err")
	var rollbackStarted int
	var rollbackCert []byte
	k.onStart = func(nc *model.NodeSpec, _ []model.UserSpec, tls kernel.TLSCert) {
		// Allow rollback Start for cfgA to succeed so portHopping completes rollback cleanly.
		if nc.ServerPort == 443 {
			rollbackStarted++
			rollbackCert = append([]byte(nil), tls.CertPEM...)
			k.startErr = nil
		}
	}

	cfgB := &model.NodeSpec{
		Protocol:   "vless",
		ServerPort: 8443,
		CertConfig: &config.CertConfig{
			CertMode:    "content",
			CertContent: string(certB),
			KeyContent:  string(keyB),
		},
	}
	s.applyPullResult(ctx, pullResult{
		config:     cfgB,
		configHash: computeConfigHash(cfgB),
	})

	if rollbackStarted != 1 {
		t.Fatalf("expected rollback Start of cfgA once, got %d", rollbackStarted)
	}
	if !bytes.Equal(rollbackCert, certA) {
		t.Fatal("expected kernel rollback to restart cfgA with certA, NOT new certB")
	}
	if !bytes.Equal(s.cert.TLSCert().CertPEM, certA) {
		t.Fatal("expected cert.Manager to be restored to certA after config rollback")
	}
}

func TestApplyUserDeltaRemoveFailureRebuildsAndRequestsResync(t *testing.T) {
	ctx := context.Background()

	// Part A: RemoveUsers + UpdateUsers fail -> startKernel(filtered) recovers cleanly.
	k := &fakeKernel{
		running:   true,
		removeErr: errors.New("partial remove err"),
		updateErr: errors.New("update fallback err"),
	}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	u1 := model.UserSpec{ID: 1, UUID: "uuid-1"}
	u2 := model.UserSpec{ID: 2, UUID: "uuid-2"}
	s.updateUserState([]model.UserSpec{u1, u2})
	s.appliedState.Users = cloneUsersNonNil([]model.UserSpec{u1, u2})

	s.applyUserDelta(ctx, "remove", []model.UserSpec{{ID: 2}})

	if k.startCalls != 1 {
		t.Fatalf("expected startKernel fallback once on double remove failure, got %d", k.startCalls)
	}
	if len(s.appliedState.Users) != 1 || s.appliedState.Users[0].ID != 1 {
		t.Fatalf("expected appliedState.Users=[1] after startKernel recovery, got %#v", s.appliedState.Users)
	}

	// Part B: Even a total failure keeps only the new target and requests reconciliation.
	k2 := &fakeKernel{
		running:   true,
		removeErr: errors.New("partial remove err"),
		updateErr: errors.New("update fallback err"),
		startErr:  errors.New("start err"),
	}
	s2 := newTestService(k2)
	s2.source = &scriptedPollingSource{
		pollFn: func(int) (controlplane.Snapshot, error) {
			return controlplane.Snapshot{}, errors.New("offline")
		},
	}
	s2.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s2.updateUserState([]model.UserSpec{u1, u2})
	s2.appliedState.Users = cloneUsersNonNil([]model.UserSpec{u1, u2})

	s2.applyUserDelta(ctx, "remove", []model.UserSpec{{ID: 2}})
	// drain async result if any failed silently
	if len(s2.lastUsers) != 1 || s2.lastUsers[0].ID != u1.ID || !s2.usersNeedsReapply {
		t.Fatalf("revoked account survived failed rebuild: %#v", s2.lastUsers)
	}
	if !s2.hasPendingForceResync() {
		t.Fatal("expected pending forced resync after remove delta + restart failure")
	}
}

func TestForceResyncSurvivesActivePullAndBackoff(t *testing.T) {
	ctx := context.Background()
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	u1 := model.UserSpec{ID: 1, UUID: "uuid-1"}
	s.updateUserState([]model.UserSpec{u1})

	// Case A: requestWSResync arrives while an ordinary pull is already active.
	entered := make(chan struct{})
	release := make(chan struct{})
	src := &scriptedPollingSource{
		pollFn: func(call int) (controlplane.Snapshot, error) {
			if call == 1 {
				close(entered)
				<-release
			}
			return controlplane.Snapshot{Users: []model.UserSpec{u1}}, nil
		},
	}
	s.source = src

	s.pullViaAPIAsync(ctx)
	<-entered
	s.requestWSResync(ctx, "missed_seq")
	if !s.hasPendingForceResync() {
		t.Fatal("expected forced resync to remain pending while ordinary pull is active")
	}

	close(release)
	res1 := <-s.pullResults
	s.applyPullResult(ctx, res1) // ordinary pull finishes, must NOT drop pending resync and must fire forced pull #2
	if res1.forced {
		t.Fatal("expected pull #1 to be ordinary (non-forced)")
	}

	select {
	case res2 := <-s.pullResults:
		if !res2.forced {
			t.Fatal("expected automatic follow-up pull #2 to be forced")
		}
		s.applyPullResult(ctx, res2)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for forced follow-up pull")
	}
	if s.hasPendingForceResync() {
		t.Fatal("expected forced resync flag to clear after forced pull succeeded")
	}

	// Case B: backoff skips forced resync -> flag stays true until backoff clears.
	s.pullBackoff.onFailure() // skipRemaining = 1
	s.requestWSResync(ctx, "backoff_test")
	if !s.hasPendingForceResync() {
		t.Fatal("expected forced resync flag to remain true when skipped by backoff")
	}
	if s.pullActive.Load() {
		t.Fatal("pullActive must be false after backoff skip")
	}
	// Next pull (when backoff allows) picks up pending forced resync and clears it.
	s.pullViaAPIWithOptions(ctx, true)
	select {
	case res3 := <-s.pullResults:
		s.applyPullResult(ctx, res3)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for forced pull after backoff cleared")
	}
	if s.hasPendingForceResync() {
		t.Fatal("expected forced resync flag to clear once post-backoff forced pull completed")
	}
}

func TestInitialSetupZeroUsersSeedsMachineMailboxBaseline(t *testing.T) {
	ctx := context.Background()
	mb := controlplane.NewNodeMailbox()

	k := &fakeKernel{}
	s := newTestService(k)
	s.cfg = &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	s.source = &zeroUserCertControlPlane{
		bootstrap: controlplane.Bootstrap{
			Config:  &model.NodeSpec{Protocol: "vless", ServerPort: 443},
			Users:   []model.UserSpec{},
			Mailbox: mb,
		},
	}

	if err := s.initialSetup(ctx); err != nil {
		t.Fatalf("initialSetup failed: %v", err)
	}
	if s.lastUsers == nil || len(s.lastUsers) != 0 {
		t.Fatalf("expected non-nil empty s.lastUsers, got %#v", s.lastUsers)
	}

	// First user arrives via WS delta add -> mailbox baseline must accept it without NeedsReconcile.
	mb.Apply(controlplane.Event{
		Type:        controlplane.EventSyncUserDelta,
		DeltaAction: "add",
		DeltaUsers:  []model.UserSpec{{ID: 1, UUID: "uuid-first"}},
	})
	s.drainMachineMailbox(ctx)

	if !k.IsRunning() || k.startCalls != 1 {
		t.Fatalf("expected first delta user to start kernel cleanly, running=%v startCalls=%d", k.IsRunning(), k.startCalls)
	}
	if s.hasPendingForceResync() {
		t.Fatal("expected zero-user machine mailbox baseline NOT to trigger REST reconcile on first user delta")
	}
}

func TestWSConfigUpdateInvalidatesConfigETagForRevert(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	panelPort := 443
	panelETag := `"etag-A"`
	var seenInm []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		port := panelPort
		etag := panelETag
		mu.Unlock()

		switch r.URL.Path {
		case "/api/v2/server/handshake":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"websocket": map[string]any{"enabled": false},
				"settings":  map[string]any{"push_interval": 60, "pull_interval": 60},
			})
		case "/api/v1/server/UniProxy/config":
			inm := r.Header.Get("If-None-Match")
			mu.Lock()
			seenInm = append(seenInm, inm)
			mu.Unlock()
			if inm == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"protocol":    "vless",
				"server_port": port,
			})
		case "/api/v1/server/UniProxy/user":
			uETag := `"etag-u"`
			if r.Header.Get("If-None-Match") == uETag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", uETag)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"users": []map[string]any{
					{"id": 1, "uuid": "uuid-1"},
				},
			})
		}
	}))
	defer srv.Close()

	k := &fakeKernel{}
	cfg := &config.Config{
		Panel:  config.PanelConfig{URL: srv.URL, Token: "tok", NodeID: 1},
		Kernel: config.KernelConfig{Type: "singbox"},
	}
	cp := controlplane.NewPanelControlPlane(cfg.Panel, cfg.WS, cfg.Kernel)
	s := newService(cfg, cp)
	s.kernel = k
	s.kernel.SetSpeedLimitFunc(s.speedTracker.GetLimiter)
	s.kernel.SetDeviceLimitFunc(s.limiter.GetDeviceLimitByUUID)

	if err := s.initialSetup(ctx); err != nil {
		t.Fatalf("initialSetup failed: %v", err)
	}
	if s.lastConfig.ServerPort != 443 {
		t.Fatalf("initial ServerPort = %d, want 443", s.lastConfig.ServerPort)
	}

	// WS pushes config B (port 8443); panel subsequently reverts to A (port 443 / etag-A) without WS delivery.
	s.handleWSEvent(ctx, controlplane.Event{
		Type:   controlplane.EventSyncConfig,
		Config: &model.NodeSpec{Protocol: "vless", ServerPort: 8443},
	})
	if s.lastConfig.ServerPort != 8443 {
		t.Fatalf("after WS config update ServerPort = %d, want 8443", s.lastConfig.ServerPort)
	}

	// Even an ordinary REST poll (without forceFull) must NOT send stale etag-A -> 304!
	s.pullViaAPIAsync(ctx)
	res := <-s.pullResults
	s.applyPullResult(ctx, res)

	if s.lastConfig.ServerPort != 443 {
		t.Fatalf("expected reverted REST config A (443) to replace WS config B, got %d", s.lastConfig.ServerPort)
	}
	mu.Lock()
	headers := append([]string(nil), seenInm...)
	mu.Unlock()
	if len(headers) != 2 || headers[1] != "" {
		t.Fatalf("expected second config GET to clear If-None-Match after WS config update, got %v", headers)
	}
}

func TestMachineMailboxABACoalesceDiscardsStaleIntermediateREST(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Part A: User ABA coalescing ([u1] -> add u2 + remove u2 -> [u1]) still discards stale REST [u1, u2].
	k := &fakeKernel{running: true}
	s := newTestService(k)
	cfg0 := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s.lastConfig = cfg0
	s.lastConfigHash = computeConfigHash(cfg0)
	u1 := model.UserSpec{ID: 1, UUID: "uuid-1"}
	u2 := model.UserSpec{ID: 2, UUID: "uuid-2"}
	s.updateUserState([]model.UserSpec{u1})
	s.appliedState.Config = cfg0
	s.appliedState.Users = cloneUsersNonNil([]model.UserSpec{u1})

	mb := controlplane.NewNodeMailbox()
	mb.SeedBaseline([]model.UserSpec{u1}, cfg0)
	mb.MarkReady()
	_ = mb.DrainIfReady()
	s.machineMailbox = mb

	poll1Entered := make(chan struct{})
	poll1Release := make(chan struct{})
	var commit1Users, commit1Config atomic.Bool

	cfg1 := &model.NodeSpec{Protocol: "vless", ServerPort: 8443}
	s.source = &scriptedPollingSource{
		pollFn: func(call int) (controlplane.Snapshot, error) {
			if call == 1 {
				close(poll1Entered)
				<-poll1Release
				return controlplane.Snapshot{
					Config:       cfg1,
					Users:        []model.UserSpec{u1, u2},
					CommitConfig: func() { commit1Config.Store(true) },
					CommitUsers:  func() { commit1Users.Store(true) },
				}, nil
			}
			return controlplane.Snapshot{
				Config: cfg0,
				Users:  []model.UserSpec{u1},
			}, nil
		},
	}

	s.pullViaAPIWithOptions(ctx, false)
	<-poll1Entered

	// Coalesce both user ABA and config ABA while REST is in flight.
	mb.Apply(controlplane.Event{Type: controlplane.EventSyncUserDelta, DeltaAction: "add", DeltaUsers: []model.UserSpec{u2}})
	mb.Apply(controlplane.Event{Type: controlplane.EventSyncUserDelta, DeltaAction: "remove", DeltaUsers: []model.UserSpec{{ID: 2}}})
	mb.Apply(controlplane.Event{Type: controlplane.EventSyncConfig, Config: cfg1})
	mb.Apply(controlplane.Event{Type: controlplane.EventSyncConfig, Config: cfg0})
	s.drainMachineMailbox(ctx)

	close(poll1Release)
	res1 := <-s.pullResults
	s.applyPullResult(ctx, res1)

	if commit1Users.Load() || commit1Config.Load() {
		t.Fatalf("ABA coalesced WS events must still invalidate stale REST commits, users=%v cfg=%v", commit1Users.Load(), commit1Config.Load())
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != 1 {
		t.Fatalf("stale REST resurrected u2 after ABA WS coalesce: %#v", s.lastUsers)
	}
	if s.lastConfig.ServerPort != 443 {
		t.Fatalf("stale REST applied intermediate cfg1 after ABA WS coalesce: port=%d", s.lastConfig.ServerPort)
	}

	// Drain follow-up forced poll #2 cleanly.
	select {
	case res2 := <-s.pullResults:
		s.applyPullResult(ctx, res2)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for forced reconciliation poll #2")
	}
}

func TestStoppedRemoveDeltaPrunesAppliedStateRollbackUsers(t *testing.T) {
	t.Setenv("XBOARD_NODE_PORT_HOPPING_STATE_DIR", t.TempDir())
	ctx := context.Background()

	k := &fakeKernel{}
	s := newTestService(k)
	s.portHopping = porthopping.New(t.TempDir())
	cfgA := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	u1 := model.UserSpec{ID: 1, UUID: "uuid-1"}
	u2 := model.UserSpec{ID: 2, UUID: "uuid-2"}
	u3 := model.UserSpec{ID: 3, UUID: "uuid-3"}

	// Initially applied [u1, u2].
	s.applyPullResult(ctx, pullResult{
		config:     cfgA,
		configHash: computeConfigHash(cfgA),
		users:      []model.UserSpec{u1, u2},
		userHash:   computeUserHash([]model.UserSpec{u1, u2}),
	})
	if len(s.appliedState.Users) != 2 {
		t.Fatalf("expected 2 applied users, got %#v", s.appliedState.Users)
	}

	// Simulate kernel currently stopped (e.g. previous restart failure).
	k.running = false

	// Partial revoke while stopped: remove u1 -> appliedState.Users shrinks to [u2].
	s.applyUserDelta(ctx, "remove", []model.UserSpec{{ID: 1}})
	if len(s.appliedState.Users) != 1 || s.appliedState.Users[0].ID != 2 {
		t.Fatalf("expected appliedState.Users pruned to [u2], got %#v", s.appliedState.Users)
	}

	// Remove last remaining applied user u2 while stopped -> appliedState.Users becomes empty/nil.
	s.applyUserDelta(ctx, "remove", []model.UserSpec{{ID: 2}})
	if len(s.appliedState.Users) != 0 {
		t.Fatalf("expected empty appliedState.Users after removing last applied user while stopped, got %#v", s.appliedState.Users)
	}

	// Now startKernel fails with a different user [u3]: rollback must NOT resurrect [u1] or [u2].
	k.startErr = errors.New("start err")
	var rollbackCalls int
	k.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		if len(users) == 1 && (users[0].ID == 1 || users[0].ID == 2) {
			rollbackCalls++
		}
	}
	s.applyUserDelta(ctx, "add", []model.UserSpec{u3})
	if rollbackCalls != 0 {
		t.Fatalf("rollback resurrected revoked users while stopped (calls=%d)", rollbackCalls)
	}

	// Also verify EventSyncUsers while stopped prunes appliedState.Users before startKernel rollback.
	s.appliedState.Users = cloneUsersNonNil([]model.UserSpec{u1, u2})
	s.lastUsers = cloneUsersNonNil([]model.UserSpec{u1, u2})
	s.applyUserUpdate(ctx, []model.UserSpec{u3}, computeUserHash([]model.UserSpec{u3}))
	if len(s.appliedState.Users) != 0 {
		t.Fatalf("expected EventSyncUsers while stopped to prune revoked users from appliedState.Users, got %#v", s.appliedState.Users)
	}
}

type tlsListenerKernel struct {
	fakeKernel
	mu       sync.Mutex
	ln       net.Listener
	addr     string
	serveErr error
}

func (k *tlsListenerKernel) closeListenerLocked() {
	if k.ln != nil {
		_ = k.ln.Close()
		k.ln = nil
		k.addr = ""
	}
}

func (k *tlsListenerKernel) listenWithTLS(tlsCert kernel.TLSCert) error {
	pair, err := tls.X509KeyPair(tlsCert.CertPEM, tlsCert.KeyPEM)
	if err != nil {
		return err
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{pair},
	})
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.closeListenerLocked()
	k.ln = ln
	k.addr = ln.Addr().String()
	k.mu.Unlock()

	go func(l net.Listener) {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if tc, ok := conn.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
			}(c)
		}
	}(ln)
	return nil
}

func (k *tlsListenerKernel) Start(nodeConfig *model.NodeSpec, users []model.UserSpec, tlsCert kernel.TLSCert) error {
	if err := k.fakeKernel.Start(nodeConfig, users, tlsCert); err != nil {
		return err
	}
	return k.listenWithTLS(tlsCert)
}

func (k *tlsListenerKernel) Reload(nodeConfig *model.NodeSpec, users []model.UserSpec, tlsCert kernel.TLSCert) error {
	if err := k.fakeKernel.Reload(nodeConfig, users, tlsCert); err != nil {
		return err
	}
	return k.listenWithTLS(tlsCert)
}

func (k *tlsListenerKernel) Stop() {
	k.fakeKernel.Stop()
	k.mu.Lock()
	k.closeListenerLocked()
	k.mu.Unlock()
}

func (k *tlsListenerKernel) handshakeCN(t *testing.T) string {
	t.Helper()
	k.mu.Lock()
	addr := k.addr
	k.mu.Unlock()
	if addr == "" {
		t.Fatal("expected active TLS listener")
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls dial %s: %v", addr, err)
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Fatal("expected server certificate in TLS handshake")
	}
	return state.PeerCertificates[0].Subject.CommonName
}

func generateLongLivedTestPEM(t *testing.T, cn string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func seedServiceACMEStorage(t *testing.T, certDir, domain string, certPEM, keyPEM []byte) {
	t.Helper()
	ctx := context.Background()
	storage := &certmagic.FileStorage{Path: certDir}
	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(_ certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})
	defer cache.Stop()
	magic = certmagic.New(cache, certmagic.Config{Storage: storage})
	issuerKey := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA: certmagic.LetsEncryptProductionCA,
	}).IssuerKey()
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certPEM); err != nil {
		t.Fatal(err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyPEM); err != nil {
		t.Fatal(err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteMeta(issuerKey, domain), []byte(fmt.Sprintf(`{"sans":[%q]}`, domain))); err != nil {
		t.Fatal(err)
	}
}

func TestACMEFailureRollbackAndRetryPresentsNewCertInTLSHandshake(t *testing.T) {
	t.Setenv("XBOARD_NODE_PORT_HOPPING_STATE_DIR", t.TempDir())
	ctx := context.Background()
	certDir := t.TempDir()
	fileDir := t.TempDir()

	certA, keyA := generateLongLivedTestPEM(t, "a.example.com")
	certAPath := filepath.Join(fileDir, "a.crt")
	keyAPath := filepath.Join(fileDir, "a.key")
	if err := os.WriteFile(certAPath, certA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyAPath, keyA, 0o600); err != nil {
		t.Fatal(err)
	}

	certB, keyB := generateLongLivedTestPEM(t, "b.example.com")
	seedServiceACMEStorage(t, certDir, "b.example.com", certB, keyB)

	var mu sync.Mutex
	activeStage := "A"
	var seenInm []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stage := activeStage
		mu.Unlock()

		switch r.URL.Path {
		case "/api/v2/server/handshake":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"websocket": map[string]any{"enabled": false},
				"settings":  map[string]any{"push_interval": 60, "pull_interval": 60},
			})
		case "/api/v1/server/UniProxy/config":
			inm := r.Header.Get("If-None-Match")
			mu.Lock()
			seenInm = append(seenInm, inm)
			mu.Unlock()
			if stage == "A" {
				if inm == `"etag-A"` {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Set("ETag", `"etag-A"`)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"protocol":    "vless",
					"server_port": 443,
					"tls":         1,
					"cert_config": map[string]any{
						"cert_mode": "file",
						"cert_file": certAPath,
						"key_file":  keyAPath,
					},
				})
				return
			}
			if inm == `"etag-B"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"etag-B"`)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"protocol":    "vless",
				"server_port": 8443,
				"tls":         1,
				"cert_config": map[string]any{
					"cert_mode": "http",
					"domain":    "b.example.com",
				},
			})
		case "/api/v1/server/UniProxy/user":
			if r.Header.Get("If-None-Match") == `"etag-u"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"etag-u"`)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"users": []map[string]any{{"id": 1, "uuid": "uuid-1"}},
			})
		}
	}))
	defer srv.Close()

	tk := &tlsListenerKernel{}
	defer tk.Stop()
	cfg := &config.Config{
		Panel:  config.PanelConfig{URL: srv.URL, Token: "tok", NodeID: 1},
		Kernel: config.KernelConfig{Type: "singbox"},
		Cert:   config.CertConfig{CertDir: certDir},
	}
	cp := controlplane.NewPanelControlPlane(cfg.Panel, cfg.WS, cfg.Kernel)
	s := newService(cfg, cp)
	s.kernel = tk
	s.portHopping = porthopping.New(t.TempDir())
	s.kernel.SetSpeedLimitFunc(s.speedTracker.GetLimiter)
	s.kernel.SetDeviceLimitFunc(s.limiter.GetDeviceLimitByUUID)
	defer s.cert.Stop()

	if err := s.initialSetup(ctx); err != nil {
		t.Fatalf("initialSetup: %v", err)
	}
	if cn := tk.handshakeCN(t); cn != "a.example.com" {
		t.Fatalf("initial TLS handshake CN = %q, want a.example.com", cn)
	}

	// Panel switches to config B (ACME b.example.com on port 8443), but kernel bind fails on 8443.
	mu.Lock()
	activeStage = "B"
	mu.Unlock()
	tk.reloadErr = errors.New("reload err")
	tk.startErr = errors.New("address already in use")
	tk.onStart = func(nc *model.NodeSpec, _ []model.UserSpec, _ kernel.TLSCert) {
		if nc.ServerPort == 443 {
			tk.startErr = nil // allow rollback to port 443
		}
	}

	s.pullViaAPIAsync(ctx)
	res1 := <-s.pullResults
	s.applyPullResult(ctx, res1)

	if s.lastConfig.ServerPort != 443 {
		t.Fatalf("expected rollback to port 443 after ACME switch kernel failure, got %d", s.lastConfig.ServerPort)
	}
	if cn := tk.handshakeCN(t); cn != "a.example.com" {
		t.Fatalf("after rollback TLS handshake CN = %q, want a.example.com", cn)
	}

	// Clear port bind failure and retry same panel config B -> must fetch 200 and serve cert B!
	tk.reloadErr = nil
	tk.startErr = nil
	tk.onStart = nil

	s.pullViaAPIAsync(ctx)
	res2 := <-s.pullResults
	s.applyPullResult(ctx, res2)

	if s.lastConfig.ServerPort != 8443 {
		t.Fatalf("expected retry to apply port 8443, got %d", s.lastConfig.ServerPort)
	}
	if cn := tk.handshakeCN(t); cn != "b.example.com" {
		t.Fatalf("after retry TLS handshake CN = %q, want b.example.com", cn)
	}
	mu.Lock()
	headers := append([]string(nil), seenInm...)
	mu.Unlock()
	if len(headers) != 3 || headers[1] != `"etag-A"` || headers[2] != `"etag-A"` {
		t.Fatalf("expected retry NOT to commit etag-B on failure, If-None-Match headers = %v", headers)
	}
}

func TestEventSyncUsersFailureRequestsWSResync(t *testing.T) {
	ctx := context.Background()
	k := &fakeKernel{
		running:   true,
		updateErr: errors.New("update err"),
		startErr:  errors.New("start err"),
	}
	s := newTestService(k)
	doneCh := make(chan struct{}, 1)
	s.source = &scriptedPollingSource{
		pollFn: func(int) (controlplane.Snapshot, error) {
			select {
			case doneCh <- struct{}{}:
			default:
			}
			return controlplane.Snapshot{}, errors.New("offline")
		},
	}
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	u1 := model.UserSpec{ID: 1, UUID: "uuid-1"}
	u2 := model.UserSpec{ID: 2, UUID: "uuid-2"}
	s.updateUserState([]model.UserSpec{u1, u2})

	s.handleWSEvent(ctx, controlplane.Event{
		Type:  controlplane.EventSyncUsers,
		Users: []model.UserSpec{u1},
	})

	if !s.hasPendingForceResync() {
		t.Fatal("expected EventSyncUsers apply failure to request WS resync")
	}
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for resync Poll call")
	}
}

func TestPanelCertOverrideClearedRestoresBaseLocalCert(t *testing.T) {
	ctx := context.Background()
	fileDir := t.TempDir()
	certA, keyA := generateLongLivedTestPEM(t, "locala.example.com")
	certAPath := filepath.Join(fileDir, "a.crt")
	keyAPath := filepath.Join(fileDir, "a.key")
	if err := os.WriteFile(certAPath, certA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyAPath, keyA, 0o600); err != nil {
		t.Fatal(err)
	}
	certB, keyB := generateLongLivedTestPEM(t, "panelb.example.com")

	// Part A: Local file cert A -> panel content override B -> panel clears cert_config -> restores file cert A.
	k := &fakeKernel{}
	baseCfg := config.CertConfig{CertMode: "file", CertFile: certAPath, KeyFile: keyAPath, CertDir: t.TempDir()}
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}, Cert: baseCfg}
	s := newService(cfg, nil)
	s.kernel = k
	if err := s.cert.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.cert.Stop()
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "uuid-1"}})

	panelBCfg := &model.NodeSpec{
		Protocol:   "vless",
		ServerPort: 443,
		TLS:        1,
		CertConfig: &config.CertConfig{CertMode: "content", CertContent: string(certB), KeyContent: string(keyB)},
	}
	s.applyPullResult(ctx, pullResult{config: panelBCfg, configHash: computeConfigHash(panelBCfg)})
	if !bytes.Equal(s.cert.TLSCert().CertPEM, certB) {
		t.Fatal("expected panel cert_config override to load cert B")
	}

	panelClearedCfg := &model.NodeSpec{Protocol: "vless", ServerPort: 443, TLS: 1}
	s.applyPullResult(ctx, pullResult{config: panelClearedCfg, configHash: computeConfigHash(panelClearedCfg)})
	if !bytes.Equal(s.cert.TLSCert().CertPEM, certA) {
		t.Fatal("expected clearing panel cert_config to restore initial local file cert A")
	}

	// Part B: Local base cert_mode=none -> panel content override B -> panel clears cert_config -> HasCert() == false.
	k2 := &fakeKernel{}
	cfg2 := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}, Cert: config.CertConfig{CertMode: "none", CertDir: t.TempDir()}}
	s2 := newService(cfg2, nil)
	s2.kernel = k2
	defer s2.cert.Stop()
	s2.updateUserState([]model.UserSpec{{ID: 1, UUID: "uuid-1"}})

	s2.applyPullResult(ctx, pullResult{config: panelBCfg, configHash: computeConfigHash(panelBCfg)})
	if !s2.cert.HasCert() {
		t.Fatal("expected panel override to populate cert B")
	}
	s2.handleWSEvent(ctx, controlplane.Event{
		Type:   controlplane.EventSyncConfig,
		Config: &model.NodeSpec{Protocol: "vless", ServerPort: 443, TLS: 0},
	})
	if s2.cert.HasCert() {
		t.Fatal("expected clearing panel cert_config when local base mode is none to clear cert material")
	}
}

func TestStaleRESTImmediateRetryBoundedUnderContinuousWSChurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	u1 := model.UserSpec{ID: 1, UUID: "uuid-1"}
	s.updateUserState([]model.UserSpec{u1})

	var pollCount atomic.Int32
	pollStarted := make(chan struct{}, 10)
	releasePoll := make(chan struct{}, 10)

	s.source = &scriptedPollingSource{
		pollFn: func(int) (controlplane.Snapshot, error) {
			pollCount.Add(1)
			pollStarted <- struct{}{}
			<-releasePoll
			return controlplane.Snapshot{Users: []model.UserSpec{u1}}, nil
		},
	}

	s.pullViaAPIWithOptions(ctx, true)
	for i := 0; i < maxImmediateStaleRetries+1; i++ {
		<-pollStarted
		// Inject a concurrent WS mutation during every in-flight REST pull.
		s.handleWSEvent(ctx, controlplane.Event{
			Type:        controlplane.EventSyncUserDelta,
			DeltaAction: "add",
			DeltaUsers:  []model.UserSpec{{ID: 100 + i, UUID: fmt.Sprintf("uuid-%d", 100+i)}},
		})
		releasePoll <- struct{}{}
		res := <-s.pullResults
		s.applyPullResult(ctx, res)
	}

	// Total polls = 1 initial + maxImmediateStaleRetries (2) = 3; no 4th immediate poll launched.
	if got := int(pollCount.Load()); got != 1+maxImmediateStaleRetries {
		t.Fatalf("expected bounded %d polls before deferring to next ticker, got %d", 1+maxImmediateStaleRetries, got)
	}
	if !s.hasPendingForceResync() {
		t.Fatal("expected pending forced resync flag to remain set for next pollTicker")
	}
	select {
	case <-pollStarted:
		t.Fatal("unexpected unbounded immediate retry launched beyond maxImmediateStaleRetries")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStoppedKernelRevocationConstrainedAcrossRollbackAndRestart(t *testing.T) {
	t.Setenv("XBOARD_NODE_PORT_HOPPING_STATE_DIR", t.TempDir())
	ctx := context.Background()

	uA := model.UserSpec{ID: 1, UUID: "uuid-A"}
	uB := model.UserSpec{ID: 2, UUID: "uuid-B"}
	cfg443 := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	cfg8443 := &model.NodeSpec{Protocol: "vless", ServerPort: 8443}
	cfg9443 := &model.NodeSpec{Protocol: "vless", ServerPort: 9443}

	// Scenario 1A: Kernel stopped with appliedState=[A,B]; combined REST delivers cfg8443 + [B].
	// Start on 8443 fails -> rollback restarts cfg443 -> MUST use [B] ONLY (no A).
	k1 := &fakeKernel{}
	s1 := newTestService(k1)
	s1.portHopping = porthopping.New(t.TempDir())
	s1.applyPullResult(ctx, pullResult{
		config:     cfg443,
		configHash: computeConfigHash(cfg443),
		users:      []model.UserSpec{uA, uB},
		userHash:   computeUserHash([]model.UserSpec{uA, uB}),
	})
	k1.running = false

	var seenStarts [][]int
	k1.startErr = errors.New("bind 8443 failed")
	k1.onStart = func(nc *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		ids := make([]int, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		seenStarts = append(seenStarts, ids)
		if nc.ServerPort == 443 {
			k1.startErr = nil // allow rollback on 443
		}
	}
	s1.applyPullResult(ctx, pullResult{
		config:     cfg8443,
		configHash: computeConfigHash(cfg8443),
		users:      []model.UserSpec{uB},
		userHash:   computeUserHash([]model.UserSpec{uB}),
	})
	if len(seenStarts) != 2 || len(seenStarts[1]) != 1 || seenStarts[1][0] != uB.ID {
		t.Fatalf("expected rollback start to use [2] only (no A=1), got starts=%v", seenStarts)
	}
	if len(s1.lastUsers) != 1 || s1.lastUsers[0].ID != uB.ID {
		t.Fatalf("expected lastUsers aligned to [2] after rollback, got %#v", s1.lastUsers)
	}

	// Scenario 1B: Both new config and rollback fail while stopped -> subsequent recovery start MUST use [B] ONLY.
	k2 := &fakeKernel{}
	s2 := newTestService(k2)
	s2.applyPullResult(ctx, pullResult{
		config:     cfg443,
		configHash: computeConfigHash(cfg443),
		users:      []model.UserSpec{uA, uB},
		userHash:   computeUserHash([]model.UserSpec{uA, uB}),
	})
	k2.running = false
	k2.startErr = errors.New("port conflict")
	s2.applyPullResult(ctx, pullResult{
		config:     cfg8443,
		configHash: computeConfigHash(cfg8443),
		users:      []model.UserSpec{uB},
		userHash:   computeUserHash([]model.UserSpec{uB}),
	})
	k2.startErr = nil
	var recoveredIDs []int
	k2.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		for _, u := range users {
			recoveredIDs = append(recoveredIDs, u.ID)
		}
	}
	s2.applyPullResult(ctx, pullResult{
		config:     cfg9443,
		configHash: computeConfigHash(cfg9443),
	})
	if len(recoveredIDs) != 1 || recoveredIDs[0] != uB.ID {
		t.Fatalf("expected first successful start after failed combined update to use [2] only, got %v", recoveredIDs)
	}

	// Scenario 2: Users-only applyUserUpdate([B]) fails while stopped -> subsequent config update (users nil / 304) MUST use [B] ONLY.
	k3 := &fakeKernel{}
	s3 := newTestService(k3)
	s3.applyPullResult(ctx, pullResult{
		config:     cfg443,
		configHash: computeConfigHash(cfg443),
		users:      []model.UserSpec{uA, uB},
		userHash:   computeUserHash([]model.UserSpec{uA, uB}),
	})
	k3.running = false
	k3.startErr = errors.New("still stopped")
	if ok := s3.applyUserUpdate(ctx, []model.UserSpec{uB}, computeUserHash([]model.UserSpec{uB})); ok {
		t.Fatal("expected applyUserUpdate to fail while startErr != nil")
	}
	k3.startErr = nil
	var nextStartIDs []int
	k3.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		for _, u := range users {
			nextStartIDs = append(nextStartIDs, u.ID)
		}
	}
	s3.applyPullResult(ctx, pullResult{
		config:     cfg8443,
		configHash: computeConfigHash(cfg8443),
	})
	if len(nextStartIDs) != 1 || nextStartIDs[0] != uB.ID {
		t.Fatalf("expected subsequent config start after stopped users-only revoke failure to use [2] only, got %v", nextStartIDs)
	}
}

func TestStoppedKernelRecoversWhenReconcileMatchesLocalHashes(t *testing.T) {
	ctx := context.Background()
	k := &fakeKernel{}
	s := newTestService(k)
	cfg0 := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	uB := model.UserSpec{ID: 2, UUID: "uuid-B"}
	s.lastConfig = cfg0
	s.lastConfigHash = computeConfigHash(cfg0)
	s.updateUserState([]model.UserSpec{uB})

	s.source = &scriptedPollingSource{
		pollFn: func(int) (controlplane.Snapshot, error) {
			return controlplane.Snapshot{
				Config: cfg0,
				Users:  []model.UserSpec{uB},
			}, nil
		},
	}

	// Poll #1 while kernel cannot bind -> hashes match locally, kernel still stopped -> MUST NOT clear forced resync.
	k.startErr = errors.New("temporary bind error")
	s.pullViaAPIWithOptions(ctx, true)
	res1 := <-s.pullResults
	s.applyPullResult(ctx, res1)

	if k.IsRunning() {
		t.Fatal("expected kernel to remain stopped while startErr != nil")
	}
	if !s.hasPendingForceResync() {
		t.Fatal("expected forced resync to remain pending when kernel is still stopped")
	}

	// Bind error clears; Poll #2 returns identical C0 + [B] -> self-heals kernel and clears resync!
	k.startErr = nil
	s.pullViaAPIWithOptions(ctx, true)
	res2 := <-s.pullResults
	s.applyPullResult(ctx, res2)

	if !k.IsRunning() {
		t.Fatal("expected reconcile with matching hashes to self-heal stopped kernel")
	}
	if s.hasPendingForceResync() {
		t.Fatal("expected forced resync flag to clear after self-heal start succeeded")
	}
}

func TestPanelCertOverrideClearedRestoresOriginalSelfSignedCert(t *testing.T) {
	ctx := context.Background()
	certDir := t.TempDir()
	k := &fakeKernel{}
	cfg := &config.Config{
		Kernel: config.KernelConfig{Type: "singbox"},
		Cert:   config.CertConfig{CertMode: "self", Domain: "selfa.example.com", CertDir: certDir},
	}
	s := newService(cfg, nil)
	s.kernel = k
	if err := s.cert.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.cert.Stop()
	origSelfPEM := append([]byte(nil), s.cert.TLSCert().CertPEM...)
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "uuid-1"}})

	certB, keyB := generateLongLivedTestPEM(t, "panelb.example.com")
	overrideCfg := &model.NodeSpec{
		Protocol:   "vless",
		ServerPort: 443,
		TLS:        1,
		CertConfig: &config.CertConfig{CertMode: "content", CertContent: string(certB), KeyContent: string(keyB)},
	}
	s.applyPullResult(ctx, pullResult{config: overrideCfg, configHash: computeConfigHash(overrideCfg)})
	if !bytes.Equal(s.cert.TLSCert().CertPEM, certB) {
		t.Fatal("expected panel override cert B to be active")
	}

	clearedCfg := &model.NodeSpec{Protocol: "vless", ServerPort: 443, TLS: 1}
	s.applyPullResult(ctx, pullResult{config: clearedCfg, configHash: computeConfigHash(clearedCfg)})
	if !bytes.Equal(s.cert.TLSCert().CertPEM, origSelfPEM) {
		t.Fatal("expected clearing panel override to restore exact original self-signed cert A, not panel cert B")
	}
}

func TestZeroUserHY2RevokingCertOverrideFailsValidationAndSkipsETag(t *testing.T) {
	ctx := context.Background()
	k := &fakeKernel{protocols: []string{"hysteria2", "vless"}}
	cfg := &config.Config{
		Kernel: config.KernelConfig{Type: "singbox"},
		Cert:   config.CertConfig{CertMode: "none", CertDir: t.TempDir()},
	}
	s := newService(cfg, nil)
	s.kernel = k
	defer s.cert.Stop()
	s.updateUserState([]model.UserSpec{}) // 0 users

	certB, keyB := generateLongLivedTestPEM(t, "hy2b.example.com")
	hy2WithCert := &model.NodeSpec{
		Protocol:   "hysteria2",
		ServerPort: 443,
		TLS:        1,
		CertConfig: &config.CertConfig{CertMode: "content", CertContent: string(certB), KeyContent: string(keyB)},
	}
	s.applyPullResult(ctx, pullResult{config: hy2WithCert, configHash: computeConfigHash(hy2WithCert)})
	if !bytes.Equal(s.cert.TLSCert().CertPEM, certB) {
		t.Fatal("expected zero-user HY2 to accept panel cert B")
	}

	// Panel clears cert_config back to none on 0-user HY2 node: must fail validation BEFORE Commit/ETag!
	hy2Revoked := &model.NodeSpec{Protocol: "hysteria2", ServerPort: 443, TLS: 1}
	committed := false
	s.applyPullResult(ctx, pullResult{
		config:       hy2Revoked,
		configHash:   computeConfigHash(hy2Revoked),
		commitConfig: func() { committed = true },
	})

	if committed {
		t.Fatal("expected zero-user HY2 revoking required cert to skip ConfigETag commit")
	}
	if !s.panelCertOverride || !bytes.Equal(s.cert.TLSCert().CertPEM, certB) {
		t.Fatal("expected cert B and panelCertOverride to be restored after failed HY2 post-override TLS validation")
	}
	if s.lastConfig.CertConfig == nil {
		t.Fatal("expected lastConfig to retain valid cert-backed HY2 config after rejection")
	}
}

func TestMachineMailboxSyncsRevocationBaselineEvenWhenApplyFails(t *testing.T) {
	ctx := context.Background()
	uA := model.UserSpec{ID: 1, UUID: "uuid-A"}
	uB := model.UserSpec{ID: 2, UUID: "uuid-B"}
	uC := model.UserSpec{ID: 3, UUID: "uuid-C"}

	k := &fakeKernel{startErr: errors.New("port busy")}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s.updateUserState([]model.UserSpec{uA, uB})

	mb := controlplane.NewNodeMailbox()
	mb.SeedBaseline([]model.UserSpec{uA, uB}, s.lastConfig)
	mb.MarkReady()
	_ = mb.DrainIfReady()
	s.machineMailbox = mb

	// Authoritative REST reconciliation sees [B] (A revoked), but local kernel start fails (usersOK=false).
	committed := false
	s.applyPullResult(ctx, pullResult{
		users:       []model.UserSpec{uB},
		userHash:    computeUserHash([]model.UserSpec{uB}),
		commitUsers: func() { committed = true },
	})
	if committed {
		t.Fatal("expected UserETag NOT to be committed when kernel start fails")
	}

	// Port clears; unrelated WS delta "add C" arrives: MUST produce [B, C], NOT resurrect revoked A!
	k.startErr = nil
	var startedIDs []int
	k.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		startedIDs = nil
		for _, u := range users {
			startedIDs = append(startedIDs, u.ID)
		}
	}
	mb.Apply(controlplane.Event{
		Type:        controlplane.EventSyncUserDelta,
		DeltaAction: "add",
		DeltaUsers:  []model.UserSpec{uC},
	})
	s.drainMachineMailbox(ctx)
	if len(startedIDs) != 0 || !s.usersNeedsReapply || len(s.lastUsers) != 2 {
		t.Fatalf("unconfirmed WS baseline must wait for REST: started=%v users=%v", startedIDs, s.lastUsers)
	}
	s.applyPullResult(ctx, pullResult{userMutSeq: s.userMutSeq, users: []model.UserSpec{uB, uC}, userHash: computeUserHash([]model.UserSpec{uB, uC})})

	if len(startedIDs) != 2 {
		t.Fatalf("expected [2, 3], got %v", startedIDs)
	}
	for _, id := range startedIDs {
		if id == uA.ID {
			t.Fatalf("revoked user A was resurrected by mailbox add delta: %v", startedIDs)
		}
	}
}

func TestStoppedDeltaAddUUIDChangeDoesNotRollbackOldUUID(t *testing.T) {
	t.Setenv("XBOARD_NODE_PORT_HOPPING_STATE_DIR", t.TempDir())
	ctx := context.Background()

	uOld := model.UserSpec{ID: 1, UUID: "uuid-old"}
	uNew := model.UserSpec{ID: 1, UUID: "uuid-new"}
	cfg443 := &model.NodeSpec{Protocol: "vless", ServerPort: 443}

	k := &fakeKernel{}
	s := newTestService(k)
	s.portHopping = porthopping.New(t.TempDir())
	s.applyPullResult(ctx, pullResult{
		config:     cfg443,
		configHash: computeConfigHash(cfg443),
		users:      []model.UserSpec{uOld},
		userHash:   computeUserHash([]model.UserSpec{uOld}),
	})
	k.running = false
	k.startErr = errors.New("stopped")
	var rolledBackToOld bool
	k.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		for _, u := range users {
			if u.UUID == "uuid-old" {
				rolledBackToOld = true
			}
		}
	}

	s.applyUserDelta(ctx, "add", []model.UserSpec{uNew})
	if rolledBackToOld {
		t.Fatal("rollback attempted to restart revoked old UUID while stopped")
	}
	if len(s.appliedState.Users) != 0 {
		t.Fatalf("expected stale old UUID pruned from appliedState.Users, got %#v", s.appliedState.Users)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-new" {
		t.Fatalf("expected lastUsers to retain uuid-new, got %#v", s.lastUsers)
	}

	k.startErr = nil
	var nextUUIDs []string
	k.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		for _, u := range users {
			nextUUIDs = append(nextUUIDs, u.UUID)
		}
	}
	s.applyPullResult(ctx, pullResult{
		config:     &model.NodeSpec{Protocol: "vless", ServerPort: 8443},
		configHash: "cfg-8443",
	})
	if len(nextUUIDs) != 0 {
		t.Fatalf("config-only recovery granted unconfirmed users: %v", nextUUIDs)
	}
	s.applyPullResult(ctx, pullResult{users: []model.UserSpec{uNew}, userHash: computeUserHash([]model.UserSpec{uNew})})
	if len(nextUUIDs) != 1 || nextUUIDs[0] != "uuid-new" {
		t.Fatalf("expected next start to use uuid-new only, got %v", nextUUIDs)
	}
}

func TestRunningKernelConfigRollbackStillAppliesUserRevocation(t *testing.T) {
	t.Setenv("XBOARD_NODE_PORT_HOPPING_STATE_DIR", t.TempDir())
	ctx := context.Background()

	uA := model.UserSpec{ID: 1, UUID: "uuid-A"}
	uB := model.UserSpec{ID: 2, UUID: "uuid-B"}
	cfg443 := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	cfg8443 := &model.NodeSpec{Protocol: "vless", ServerPort: 8443}

	k := &fakeKernel{}
	s := newTestService(k)
	s.portHopping = porthopping.New(t.TempDir())
	s.applyPullResult(ctx, pullResult{
		config:     cfg443,
		configHash: computeConfigHash(cfg443),
		users:      []model.UserSpec{uA, uB},
		userHash:   computeUserHash([]model.UserSpec{uA, uB}),
	})

	var startHistory [][]int
	k.reloadErr = errors.New("reload 8443 failed")
	k.startErr = errors.New("bind 8443 failed")
	k.onStart = func(nc *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		ids := make([]int, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		startHistory = append(startHistory, ids)
		if nc.ServerPort == 443 {
			k.startErr = nil
		}
	}

	var updatedTo []int
	k.onUpdateUsers = func(users []model.UserSpec) {
		updatedTo = nil
		for _, u := range users {
			updatedTo = append(updatedTo, u.ID)
		}
	}

	cfgCommitted, usersCommitted := false, false
	s.applyPullResult(ctx, pullResult{
		config:       cfg8443,
		configHash:   computeConfigHash(cfg8443),
		users:        []model.UserSpec{uB},
		userHash:     computeUserHash([]model.UserSpec{uB}),
		commitConfig: func() { cfgCommitted = true },
		commitUsers:  func() { usersCommitted = true },
	})

	if cfgCommitted {
		t.Fatal("expected failed cfg8443 NOT to commit ConfigETag")
	}
	if !usersCommitted {
		t.Fatal("expected successful user revocation on restored cfg443 to commit UserETag")
	}
	if len(startHistory) != 2 || len(startHistory[1]) != 1 || startHistory[1][0] != uB.ID {
		t.Fatalf("expected rollback Start to launch [2] only without A, got %v", startHistory)
	}
	if len(updatedTo) != 1 || updatedTo[0] != uB.ID {
		t.Fatalf("expected UpdateUsers to enforce [2] only on restored running kernel, got %v", updatedTo)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != uB.ID {
		t.Fatalf("expected lastUsers=[2], got %#v", s.lastUsers)
	}
}

func TestSelfHealPartialRollbackRealignsLastUsersForNextReconcile(t *testing.T) {
	t.Setenv("XBOARD_NODE_PORT_HOPPING_STATE_DIR", t.TempDir())
	ctx := context.Background()

	uB := model.UserSpec{ID: 2, UUID: "uuid-B"}
	uC := model.UserSpec{ID: 3, UUID: "uuid-C"}
	cfg0 := &model.NodeSpec{Protocol: "vless", ServerPort: 443}

	k := &fakeKernel{}
	s := newTestService(k)
	s.portHopping = porthopping.New(t.TempDir())

	// Seed appliedState=[B] so rollback starts [B], then simulate stopped kernel targeting [B, C].
	s.applyPullResult(ctx, pullResult{
		config:     cfg0,
		configHash: computeConfigHash(cfg0),
		users:      []model.UserSpec{uB},
		userHash:   computeUserHash([]model.UserSpec{uB}),
	})
	k.running = false
	s.updateUserState([]model.UserSpec{uB, uC})

	usersCommitted1 := false
	k.startErr = errors.New("cannot start with C")
	k.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		if len(users) == 1 && users[0].ID == uB.ID {
			k.startErr = nil // allow rollback start of [B]
		}
	}
	s.source = &scriptedPollingSource{
		pollFn: func(int) (controlplane.Snapshot, error) {
			return controlplane.Snapshot{
				Config:      cfg0,
				Users:       []model.UserSpec{uB, uC},
				CommitUsers: func() { usersCommitted1 = true },
			}, nil
		},
	}

	s.pullViaAPIWithOptions(ctx, true)
	res1 := <-s.pullResults
	s.applyPullResult(ctx, res1)

	if !k.IsRunning() {
		t.Fatal("expected rollback to keep kernel running [B]")
	}
	if usersCommitted1 {
		t.Fatal("partial rollback to [B] must NOT commit UserETag for [B, C]")
	}
	if !s.hasPendingForceResync() {
		t.Fatal("partial rollback to [B] must NOT mark forced resync done")
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != uB.ID {
		t.Fatalf("expected lastUsers realigned to [B] after partial rollback, got %#v", s.lastUsers)
	}

	// Next poll with identical [B, C] now sees hash diff ([B] vs [B, C]) and adds C cleanly!
	usersCommitted2 := false
	k.startErr = nil
	k.onStart = nil
	s.source = &scriptedPollingSource{
		pollFn: func(int) (controlplane.Snapshot, error) {
			return controlplane.Snapshot{
				Config:      cfg0,
				Users:       []model.UserSpec{uB, uC},
				CommitUsers: func() { usersCommitted2 = true },
			}, nil
		},
	}
	s.pullViaAPIWithOptions(ctx, true)
	res2 := <-s.pullResults
	s.applyPullResult(ctx, res2)

	if !usersCommitted2 || s.hasPendingForceResync() {
		t.Fatal("expected second reconcile to apply [B, C], commit ETag, and finish forced resync")
	}
	if len(s.appliedState.Users) != 2 {
		t.Fatalf("expected appliedState.Users=[B, C], got %#v", s.appliedState.Users)
	}
}

func TestBufferedWSArbitrationPrecedesEveryKernelUserApply(t *testing.T) {
	a := model.UserSpec{ID: 1, UUID: "a"}
	b := model.UserSpec{ID: 2, UUID: "b"}
	c := model.UserSpec{ID: 3, UUID: "c"}
	nextB := model.UserSpec{ID: 2, UUID: "replacement"}
	cases := []struct {
		name       string
		ws         []controlplane.Event
		rest, want []model.UserSpec
	}{
		{"buffered add vs REST withdraw", []controlplane.Event{{Type: controlplane.EventSyncUserDelta, DeltaAction: "add", DeltaUsers: []model.UserSpec{c}}}, []model.UserSpec{b}, []model.UserSpec{b}},
		{"buffered remove vs REST grant", []controlplane.Event{{Type: controlplane.EventSyncUserDelta, DeltaAction: "remove", DeltaUsers: []model.UserSpec{b}}}, []model.UserSpec{b}, []model.UserSpec{}},
		{"full then UUID swap", []controlplane.Event{{Type: controlplane.EventSyncUsers, Users: []model.UserSpec{b}}, {Type: controlplane.EventSyncUserDelta, DeltaAction: "add", DeltaUsers: []model.UserSpec{nextB}}}, []model.UserSpec{b}, []model.UserSpec{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestService(&fakeKernel{})
			k := s.kernel.(*fakeKernel)
			cfg := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
			s.applyPullResult(context.Background(), pullResult{config: cfg, configHash: computeConfigHash(cfg), users: []model.UserSpec{a, b}, userHash: computeUserHash([]model.UserSpec{a, b})})
			s.machineMailbox = controlplane.NewNodeMailbox()
			s.machineMailbox.SeedBaseline(s.lastUsers, cfg)
			s.machineMailbox.MarkReady()
			for _, e := range tc.ws {
				s.machineMailbox.Apply(e)
			}
			check := func(users []model.UserSpec) {
				if computeUserHash(users) != computeUserHash(tc.want) {
					t.Fatalf("intermediate kernel apply: %v, want %v", users, tc.want)
				}
			}
			k.onUpdateUsers = check
			k.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) { check(users) }
			s.wsResyncWanted.Store(1)
			s.applyPullResult(context.Background(), pullResult{forced: true, resyncGen: 1, users: tc.rest, userHash: computeUserHash(tc.rest), commitUsers: func() { t.Fatal("conflicting ETag committed") }})
			check(s.lastUsers)
			if !s.hasPendingForceResync() || s.lastUsers == nil || (len(tc.want) == 0 && k.IsRunning()) {
				t.Fatal("conflict lost its full recheck or explicit empty list")
			}
		})
	}
}

func TestUncommittedTargetCannotRestartFrom304(t *testing.T) {
	b := model.UserSpec{ID: 1, UUID: "retained"}
	c := model.UserSpec{ID: 2, UUID: "temporarily-granted"}
	ctx := context.Background()
	k := &fakeKernel{}
	s := newTestService(k)
	cfg := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s.applyPullResult(ctx, pullResult{config: cfg, configHash: computeConfigHash(cfg), users: []model.UserSpec{b}, userHash: computeUserHash([]model.UserSpec{b})})
	k.updateErr, k.startErr = errors.New("partial update"), errors.New("cannot start")
	s.applyPullResult(ctx, pullResult{users: []model.UserSpec{b, c}, userHash: computeUserHash([]model.UserSpec{b, c}), commitUsers: func() { t.Fatal("failed ETag committed") }})
	if !s.usersNeedsReapply {
		t.Fatal("missing reapply marker")
	}
	k.updateErr, k.startErr = nil, nil
	k.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		if computeUserHash(users) != computeUserHash([]model.UserSpec{b}) {
			t.Fatalf("unsafe restart: %v", users)
		}
	}
	other := &model.NodeSpec{Protocol: "vless", ServerPort: 8443}
	s.applyPullResult(ctx, pullResult{config: other, configHash: computeConfigHash(other), commitUsers: func() { t.Fatal("304 cleared unconfirmed target") }})
	if !s.usersNeedsReapply || !s.hasPendingForceResync() {
		t.Fatal("304 must retain forced reconciliation")
	}
	committed := false
	s.applyPullResult(ctx, pullResult{forced: true, resyncGen: s.wsResyncWanted.Load(), users: []model.UserSpec{b}, userHash: computeUserHash([]model.UserSpec{b}), commitUsers: func() { committed = true }})
	if !committed || s.usersNeedsReapply || s.hasPendingForceResync() || !k.IsRunning() {
		t.Fatal("fresh exact user list did not restore service")
	}
}

func TestBootstrapArbitratesBeforeOpeningListener(t *testing.T) {
	a := model.UserSpec{ID: 1, UUID: "a"}
	mb := controlplane.NewNodeMailbox()
	mb.Apply(controlplane.Event{Type: controlplane.EventSyncUserDelta, DeltaAction: "remove", DeltaUsers: []model.UserSpec{a}})
	cfg := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	k := &fakeKernel{}
	s := newTestService(k)
	s.source = &zeroUserCertControlPlane{bootstrap: controlplane.Bootstrap{Config: cfg, Users: []model.UserSpec{a}, Mailbox: mb, CommitUsers: func() { t.Fatal("unconfirmed initial ETag committed") }}}
	if err := s.initialSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.startCalls != 0 || s.lastUsers == nil || len(s.lastUsers) != 0 {
		t.Fatalf("initial listener opened with stale accounts: calls=%d users=%v", k.startCalls, s.lastUsers)
	}
}

func TestAckOnlyRenewalActuallyLoaded(t *testing.T) {
	ctx := context.Background()
	k := &fakeKernel{}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "a"}})
	s.cert.MarkRenewed()
	k.onStart = func(_ *model.NodeSpec, _ []model.UserSpec, _ kernel.TLSCert) { s.cert.MarkRenewed() }
	s.handleCertRenewal(ctx)
	if !s.cert.HasPendingRenewal() {
		t.Fatal("ack swallowed the later renewal")
	}
	k.onStart = nil
	s.handleCertRenewal(ctx)
	if s.cert.HasPendingRenewal() {
		t.Fatal("newer renewal did not finish")
	}
}

// Drive a real PanelControlPlane through E0, failed E1, return to E0, and 304.
// Only the newest body may recover an uncertain kernel.
func TestUserETagRecoveryWithRealHTTP(t *testing.T) {
	var version atomic.Int32
	var headersMu sync.Mutex
	var headers []string
	data := []string{
		"{\"users\":[{\"id\":1,\"uuid\":\"b\"}]}",
		"{\"users\":[{\"id\":1,\"uuid\":\"b\"},{\"id\":2,\"uuid\":\"c\"}]}",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/handshake":
			fmt.Fprint(w, "{\"websocket\":{\"enabled\":false}}")
		case "/api/v1/server/UniProxy/config":
			if r.Header.Get("If-None-Match") == `"config"` {
				w.WriteHeader(304)
				return
			}
			w.Header().Set("ETag", `"config"`)
			fmt.Fprint(w, "{\"protocol\":\"vless\",\"server_port\":443}")
		case "/api/v1/server/UniProxy/user":
			inm := r.Header.Get("If-None-Match")
			headersMu.Lock()
			headers = append(headers, inm)
			headersMu.Unlock()
			v := version.Load()
			etag := fmt.Sprintf(`"E%d"`, v)
			if inm == etag {
				w.WriteHeader(304)
				return
			}
			w.Header().Set("ETag", etag)
			fmt.Fprint(w, data[v])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	cfg := &config.Config{Panel: config.PanelConfig{URL: srv.URL, Token: "token", NodeID: 1}, Kernel: config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir()}}
	cp := controlplane.NewPanelControlPlane(cfg.Panel, cfg.WS, cfg.Kernel)
	k := &fakeKernel{}
	s := newTestService(k)
	s.cfg, s.source = cfg, cp
	if err := s.initialSetup(ctx); err != nil {
		t.Fatal(err)
	}
	version.Store(1)
	s.pullViaAPIAsync(ctx)
	e1 := <-s.pullResults
	if len(e1.users) != 2 {
		t.Fatalf("E1 lost its body: %v", e1.users)
	}
	// Make E0 current again before the forced recovery is dispatched.
	version.Store(0)
	k.updateErr, k.startErr = errors.New("partial"), errors.New("busy")
	s.applyPullResult(ctx, e1)
	if !s.usersNeedsReapply {
		t.Fatal("E1 failure was not recorded")
	}
	recovery := <-s.pullResults
	if !recovery.forced || len(recovery.users) != 1 || recovery.users[0].UUID != "b" {
		t.Fatalf("wrong recovery body: %v", recovery.users)
	}
	k.updateErr, k.startErr = nil, nil
	k.onStart = func(_ *model.NodeSpec, users []model.UserSpec, _ kernel.TLSCert) {
		if len(users) != 1 || users[0].UUID != "b" {
			t.Fatalf("unauthorized recovery listener: %v", users)
		}
	}
	s.applyPullResult(ctx, recovery)
	if s.usersNeedsReapply || s.hasPendingForceResync() || !k.IsRunning() {
		t.Fatal("fresh E0 did not restore users")
	}
	s.pullViaAPIAsync(ctx)
	unchanged := <-s.pullResults
	if unchanged.users != nil {
		t.Fatalf("expected 304/no body, got %v", unchanged.users)
	}
	s.applyPullResult(ctx, unchanged)
	headersMu.Lock()
	defer headersMu.Unlock()
	if fmt.Sprint(headers) != fmt.Sprint([]string{"", `"E0"`, "", `"E0"`}) {
		t.Fatalf("If-None-Match headers: %#v", headers)
	}
}

func TestMailboxConflictRetriesAreBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b := model.UserSpec{ID: 1, UUID: "b"}
	users := []model.UserSpec{b}
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s.updateUserState(users)
	mb := controlplane.NewNodeMailbox()
	mb.SeedBaseline(users, s.lastConfig)
	mb.MarkReady()
	s.machineMailbox = mb
	src := &scriptedPollingSource{pollFn: func(int) (controlplane.Snapshot, error) {
		mb.Apply(controlplane.Event{Type: controlplane.EventSyncUsers, Users: users})
		return controlplane.Snapshot{Users: users, CommitUsers: func() { t.Fatal("overlap committed ETag") }}, nil
	}}
	s.source = src
	s.pullViaAPIWithOptions(ctx, true)
	for i := 0; i <= maxImmediateStaleRetries; i++ {
		select {
		case r := <-s.pullResults:
			s.applyPullResult(ctx, r)
		case <-ctx.Done():
			t.Fatal("expected automatic overlap retry")
		}
	}
	if s.pullActive.Load() || !s.hasPendingForceResync() || s.staleImmediateRetries != maxImmediateStaleRetries {
		t.Fatal("overlaps must defer further work until the next ticker")
	}
}

func TestCombinedMachineBatchNeverReloadsRevokedUser(t *testing.T) {
	b, c := model.UserSpec{ID: 1, UUID: "b"}, model.UserSpec{ID: 2, UUID: "c"}
	for _, source := range []string{"REST and queued WS", "coalesced WS"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			k := &fakeKernel{}
			s := newTestService(k)
			old := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
			users := []model.UserSpec{b, c}
			s.applyPullResult(ctx, pullResult{config: old, configHash: computeConfigHash(old), users: users, userHash: computeUserHash(users)})
			mb := controlplane.NewNodeMailbox()
			mb.SeedBaseline(users, old)
			mb.MarkReady()
			s.machineMailbox = mb
			mb.Apply(controlplane.Event{Type: controlplane.EventSyncUserDelta, DeltaAction: "remove", DeltaUsers: []model.UserSpec{c}})
			confirm := func(list []model.UserSpec) {
				if len(list) != 1 || list[0].UUID != "b" {
					t.Fatalf("kernel operation included revoked user: %v", list)
				}
			}
			k.onStart = func(_ *model.NodeSpec, list []model.UserSpec, _ kernel.TLSCert) { confirm(list) }
			k.onUpdateUsers = confirm
			k.onReload = confirm
			next := &model.NodeSpec{Protocol: "vless", ServerPort: 8443}
			if source == "coalesced WS" {
				mb.Apply(controlplane.Event{Type: controlplane.EventSyncConfig, Config: next})
				s.drainMachineMailbox(ctx)
			} else {
				s.applyPullResult(ctx, pullResult{config: next, configHash: computeConfigHash(next), users: users, userHash: computeUserHash(users), commitUsers: func() { t.Fatal("committed intersected REST user ETag") }})
			}
			if k.reloadCalls == 0 || s.lastConfig.ServerPort != 8443 {
				t.Fatal("new config was not applied")
			}
			confirm(s.lastUsers)
		})
	}
}
