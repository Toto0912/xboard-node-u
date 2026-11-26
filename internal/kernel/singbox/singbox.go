package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	N "github.com/sagernet/sing/common/network"
	"io"
	"strconv"
	"sync"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	singJSON "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"golang.org/x/time/rate"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/nlog"
)

// SingBox implements kernel.Kernel by embedding sing-box as a Go library.
//
// Supported protocols use the compatibility patch's UpdatableInbound interface
// to change credentials without restarting listeners. Other protocols rebuild
// the instance with rollback on failure.
type SingBox struct {
	cfg config.KernelConfig

	mu     sync.RWMutex
	box    *box.Box
	ctx    context.Context
	cancel context.CancelFunc

	users      []model.UserSpec
	nodeConfig *model.NodeSpec
	tls        kernel.TLSCert

	// connTracker is our lightweight in-process byte/IP tracker.
	// Created fresh on every Start (full restart).
	// Survives Reload (hot-swap) since live connections persist.
	connTracker *ConnTracker
	sessions    *kernel.SessionDrain

	// speedLimitFunc resolves a user UUID to a *rate.Limiter.
	// Set once by SetSpeedLimitFunc and forwarded to every new ConnTracker.
	speedLimitFunc func(string) *rate.Limiter

	// deviceLimitFunc resolves a user UUID to (limit, hasLimit) for gate-keeping.
	// Set once by SetDeviceLimitFunc and forwarded to every new ConnTracker.
	deviceLimitFunc func(string) (int, bool)
	userPolicy      *kernel.UserPolicy

	// trackerRegistered prevents duplicate AppendTracker calls on the same
	// Router instance during Reload. Reset to false on full restart.
	trackerRegistered bool
}

func New(cfg config.KernelConfig) *SingBox {
	return &SingBox{cfg: cfg}
}

var _ kernel.Kernel = (*SingBox)(nil)

func (s *SingBox) Name() string { return "sing-box" }

func (s *SingBox) Capabilities() kernel.Capabilities {
	return kernel.Capabilities{
		PerUserSpeedLimit:    true,
		DeviceLimit:          true,
		BuiltInTrafficStats:  false,
		AliveIPTracking:      true,
		ForceCloseConnection: false,
		ForceCloseUser:       true,
	}
}

func (s *SingBox) Protocols() []string {
	return []string{
		"vmess", "vless", "trojan", "shadowsocks",
		"hysteria", "hysteria2", "tuic", "naive", "socks", "http", "anytls", "mieru",
	}
}

func (s *SingBox) Start(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box != nil {
		return s.reloadLocked(nodeConfig, users, tls)
	}
	return s.startLocked(nodeConfig, users, tls)
}

func (s *SingBox) startLocked(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	cfgMap := buildConfig(s.cfg, nodeConfig, users, tls)
	data, err := json.Marshal(cfgMap)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	nlog.Core().Debug("sing-box config generated", "len", len(data))

	ctx, cancel := context.WithCancel(context.Background())
	ctx = include.Context(ctx)

	opts, err := singJSON.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		cancel()
		return fmt.Errorf("parse sing-box options: %w", err)
	}

	instance, err := box.New(box.Options{
		Context: ctx,
		Options: opts,
	})
	if err != nil {
		cancel()
		return fmt.Errorf("create sing-box instance: %w", err)
	}

	// Preserve cumulative counters across configuration reloads.
	if s.connTracker == nil {
		s.connTracker = NewConnTracker(0)
	}
	s.connTracker.userPolicy.Store(s.userPolicy)
	setTrackerUsers(s.connTracker, nodeConfig.Protocol, users)
	if s.speedLimitFunc != nil {
		s.connTracker.SetSpeedLimitFunc(s.speedLimitFunc)
	}
	if s.deviceLimitFunc != nil {
		s.connTracker.SetDeviceLimitFunc(s.deviceLimitFunc)
	}

	s.trackerRegistered = false
	s.registerTracker(ctx)
	// Register accounting before any listener can accept traffic.
	if err := instance.Start(); err != nil {
		instance.Close()
		cancel()
		return fmt.Errorf("start sing-box: %w", err)
	}
	s.box = instance
	s.ctx = ctx
	s.cancel = cancel
	s.users = users
	s.nodeConfig = nodeConfig
	s.tls = tls

	nlog.Core().Debug("sing-box started", "users", len(users))
	return nil
}

// Reload rebuilds the box when node configuration changes. User-only changes
// use UpdateUsers and keep existing connections and listeners alive.
func (s *SingBox) Reload(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked(nodeConfig, users, tls)
}

func (s *SingBox) reloadLocked(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	if s.box == nil {
		return fmt.Errorf("not running")
	}
	// Validate before releasing the current listeners.
	data, err := json.Marshal(buildConfig(s.cfg, nodeConfig, users, tls))
	if err != nil {
		return err
	}
	if _, err = singJSON.UnmarshalExtendedContext[option.Options](s.ctx, data); err != nil {
		return fmt.Errorf("parse options: %w", err)
	}
	oldNode, oldUsers, oldTLS := s.nodeConfig, s.users, s.tls
	s.stop()
	if err := s.startLocked(nodeConfig, users, tls); err != nil {
		allowed := model.IntersectUsers(oldUsers, users)
		if len(allowed) == 0 {
			return err
		}
		if rollbackErr := s.startLocked(oldNode, allowed, oldTLS); rollbackErr != nil {
			return fmt.Errorf("reload: %v; restore previous config: %w", err, rollbackErr)
		}
		return err
	}
	return nil
}

// registerTracker wires the ConnTracker to the current Router exactly once.
// The ConnTracker handles both byte counting and optional rate limiting
// in a single wrapper, so no additional trackers are needed.
func (s *SingBox) registerTracker(ctx context.Context) {
	if s.trackerRegistered {
		return
	}
	router := service.FromContext[adapter.Router](ctx)
	if router == nil {
		return
	}
	if s.connTracker != nil {
		s.sessions = new(kernel.SessionDrain)
		router.AppendTracker(&scopedTracker{s.connTracker, s.sessions})
	}
	s.trackerRegistered = true
}

type scopedTracker struct {
	*ConnTracker
	sessions *kernel.SessionDrain
}

func (t *scopedTracker) TrackSession(conn io.Closer, onClose N.CloseHandlerFunc) (N.CloseHandlerFunc, bool) {
	finish, ok := t.sessions.Begin(func() { _ = conn.Close() })
	if !ok {
		return nil, false
	}
	return N.OnceClose(func(err error) {
		defer finish()
		if onClose != nil {
			onClose(err)
		}
	}), true
}

func (s *SingBox) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop()
}

func (s *SingBox) stop() {
	if s.box == nil {
		return
	}

	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if !s.sessions.CloseAndWait() {
		nlog.Core().Warn("sing-box: session drain timed out; reporting available counters")
	}
	s.box.Close()
	s.box = nil
	s.ctx = nil
}

func (s *SingBox) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.box != nil
}

// connTrackerLocked returns the connTracker under a read-optimised lock.
func (s *SingBox) connTrackerSafe() *ConnTracker {
	s.mu.Lock()
	ct := s.connTracker
	s.mu.Unlock()
	return ct
}

// SetSpeedLimitFunc configures per-user bandwidth throttling.
func (s *SingBox) SetSpeedLimitFunc(fn func(uuid string) *rate.Limiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speedLimitFunc = fn
	if s.connTracker != nil {
		s.connTracker.SetSpeedLimitFunc(fn)
	}
}

func (s *SingBox) SetUserPolicy(p *kernel.UserPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userPolicy = p
	if s.connTracker != nil {
		s.connTracker.userPolicy.Store(p)
	}
}

// SetDeviceLimitFunc configures per-user device limit gate-keeping.
// Connections exceeding the limit are rejected at connect time.
func (s *SingBox) SetDeviceLimitFunc(fn func(uuid string) (int, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceLimitFunc = fn
	if s.connTracker != nil {
		s.connTracker.SetDeviceLimitFunc(fn)
	}
}

// UpdateGlobalDevices updates the global device state from panel (for multi-node).
func (s *SingBox) UpdateGlobalDevices(users map[int][]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.connTracker != nil {
		s.connTracker.UpdateGlobalDevices(users)
	}
}

// ClearGlobalDevices clears the global device state (on WS disconnect).
func (s *SingBox) ClearGlobalDevices() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.connTracker != nil {
		s.connTracker.ClearGlobalDevices()
	}
}

// ─── User management (non-disruptive) ───────────────────────────────────────

// AddUsers hot-swaps users into running inbounds. Zero connection disruption.
func (s *SingBox) AddUsers(users []model.UserSpec) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return 0, fmt.Errorf("not running")
	}

	existing := make(map[int]struct{}, len(s.users))
	for _, u := range s.users {
		existing[u.ID] = struct{}{}
	}
	var toAdd []model.UserSpec
	for _, u := range users {
		if _, dup := existing[u.ID]; !dup {
			toAdd = append(toAdd, u)
		}
	}
	if len(toAdd) == 0 {
		return 0, nil
	}

	merged := append(append([]model.UserSpec{}, s.users...), toAdd...)
	if err := s.reloadInboundsLocked(merged); err != nil {
		return 0, err
	}
	s.users = merged
	return len(toAdd), nil
}

// RemoveUsers hot-swaps users out of running inbounds. Zero connection disruption
// for remaining users.
func (s *SingBox) RemoveUsers(users []model.UserSpec) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return 0, fmt.Errorf("not running")
	}

	removeSet := make(map[int]struct{}, len(users))
	for _, u := range users {
		removeSet[u.ID] = struct{}{}
	}
	var kept []model.UserSpec
	removed := 0
	for _, u := range s.users {
		if _, rm := removeSet[u.ID]; rm {
			removed++
		} else {
			kept = append(kept, u)
		}
	}
	if removed == 0 {
		return 0, nil
	}

	if err := s.reloadInboundsLocked(kept); err != nil {
		return 0, err
	}
	s.users = kept
	return removed, nil
}

// UpdateUsers replaces the entire user set atomically via hot-swap.
func (s *SingBox) UpdateUsers(users []model.UserSpec) (added, removed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return 0, 0, fmt.Errorf("not running")
	}

	toAdd, toRemove := kernel.UserDiff(s.users, users)
	added, removed = len(toAdd), len(toRemove)

	if added == 0 && removed == 0 {
		// Only limits may have changed — update tracker map.
		if s.connTracker != nil {
			setTrackerUsers(s.connTracker, s.nodeConfig.Protocol, users)
		}
		s.users = users
		return 0, 0, nil
	}

	if err = s.reloadInboundsLocked(users); err != nil {
		return 0, 0, err
	}
	s.users = users
	return
}

// Decode only the account records produced by the same inbound builder as a cold start.
func updateEncodedUsers[T any](data []byte, update func([]T) error) error {
	var users []T
	if err := json.Unmarshal(data, &users); err != nil {
		return err
	}
	if len(users) == 0 {
		// The kernel's full configuration decoder represents an empty list as nil.
		users = nil
	}
	return update(users)
}

// reloadInboundsLocked hot-swaps inbound users using UpdatableInbound.
// Must be called with s.mu held.
func (s *SingBox) reloadInboundsLocked(users []model.UserSpec) error {
	inbound := buildInbound(s.nodeConfig, users, s.tls)
	if inbound == nil {
		return s.reloadLocked(s.nodeConfig, users, s.tls)
	}
	im := service.FromContext[adapter.InboundManager](s.ctx)
	if im == nil {
		return fmt.Errorf("inbound manager not available")
	}
	tag, _ := inbound["tag"].(string)
	existing, ok := im.Get(tag)
	if !ok || existing.Type() != inbound["type"] {
		return s.reloadLocked(s.nodeConfig, users, s.tls)
	}
	data, err := json.Marshal(inbound["users"])
	if err != nil {
		return fmt.Errorf("marshal inbound users: %w", err)
	}
	switch v := existing.(type) {
	case adapter.UpdatableInbound[option.VMessUser]:
		err = updateEncodedUsers(data, v.UpdateUsers)
	case adapter.UpdatableInbound[option.VLESSUser]:
		err = updateEncodedUsers(data, v.UpdateUsers)
	case adapter.UpdatableInbound[option.TrojanUser]:
		err = updateEncodedUsers(data, v.UpdateUsers)
	case adapter.UpdatableInbound[option.Hysteria2User]:
		err = updateEncodedUsers(data, v.UpdateUsers)
	case adapter.UpdatableShadowsocksInbound:
		err = updateEncodedUsers(data, v.UpdateUsersByOptions)
	case adapter.UpdatableInbound[option.TUICUser]:
		err = updateEncodedUsers(data, v.UpdateUsers)
	case adapter.UpdatableInbound[option.AnyTLSUser]:
		err = updateEncodedUsers(data, v.UpdateUsers)
	case adapter.UpdatableInbound[option.MieruUser]:
		err = updateEncodedUsers(data, v.UpdateUsers)
	case adapter.UpdatableInbound[auth.User]:
		err = updateEncodedUsers(data, v.UpdateUsers)
	default:
		err = fmt.Errorf("live user update unsupported")
	}
	if err != nil {
		nlog.Core().Warn("incremental update unavailable, rebuilding kernel", "tag", tag, "error", err)
		return s.reloadLocked(s.nodeConfig, users, s.tls)
	}

	if s.connTracker != nil {
		setTrackerUsers(s.connTracker, s.nodeConfig.Protocol, users)
	}

	nlog.Core().Debug("sing-box users hot-swapped", "users", len(users))
	return nil
}

// ─── Observability ──────────────────────────────────────────────────────────
func (s *SingBox) GetUserTraffic(_ context.Context) (traffic map[int][2]int64, aliveIPs map[int]map[string]bool, connCount int, err error) {
	ct := s.connTrackerSafe()
	if ct == nil {
		return nil, nil, 0, nil
	}
	traffic, aliveIPs, connCount = ct.GetUserTraffic()
	return traffic, aliveIPs, connCount, nil
}

// CloseConnection force-closes a specific connection by its ID.
func (s *SingBox) CloseConnection(_ context.Context, connID string) error {
	ct := s.connTrackerSafe()
	if ct == nil {
		return fmt.Errorf("not running")
	}
	if !ct.CloseByID(connID) {
		nlog.Core().Debug("CloseConnection: connection not found (already closed?)", "id", connID)
	}
	return nil
}

// CloseUserConnections terminates all connections for a user UUID.
func (s *SingBox) CloseUserConnections(_ context.Context, uuid string) error {
	ct := s.connTrackerSafe()
	if ct == nil {
		return nil
	}
	ct.CloseByUUID(uuid)
	return nil
}

// Naive exposes the numeric login to the router. Limits are keyed by UUID for every protocol.
func setTrackerUsers(t *ConnTracker, protocol string, users []model.UserSpec) {
	ids := make(map[string]int, len(users))
	var names map[string]string
	if protocol == "naive" {
		names = make(map[string]string, len(users))
	}
	for _, u := range users {
		ids[u.UUID] = u.ID
		if names != nil {
			names[strconv.Itoa(u.ID)] = u.UUID
		}
	}
	if protocol == "hysteria2" {
		protocol = "hysteria"
	}
	t.setUserMaps(ids, names, protocol+"-in")
}
