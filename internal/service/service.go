package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cedar2025/xboard-node/internal/cert"
	"github.com/cedar2025/xboard-node/internal/cert/dnsproviders"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/kernel/singbox"
	"github.com/cedar2025/xboard-node/internal/kernel/xray"
	"github.com/cedar2025/xboard-node/internal/limiter"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/monitor"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/porthopping"
	"github.com/cedar2025/xboard-node/internal/tracker"
)

const maxImmediateStaleRetries = 2

type Service struct {
	cfg          *config.Config
	source       controlplane.Source
	sink         controlplane.Sink
	kernel       kernel.Kernel
	portHopping  *porthopping.Manager
	tracker      *tracker.Tracker
	batchDone    chan struct{}
	batchDisk    sync.Mutex
	batchStats   atomic.Pointer[spoolStatus]
	limiter      *limiter.Limiter
	speedTracker *limiter.SpeedTracker
	userPolicy   kernel.UserPolicy
	cert         *cert.Manager

	baseCertCfg       config.CertConfig
	baseCertInit      bool
	panelCertOverride bool

	lastConfig *model.NodeSpec
	lastUsers  []model.UserSpec

	// nodeLog is the logger with node context for this service instance.
	nodeLog *nlog.NodeLog

	// appliedState tracks the configuration, users, and TLS certificate that
	// are currently successfully running in the kernel.
	appliedState struct {
		Config  *model.NodeSpec
		Users   []model.UserSpec
		TLSCert kernel.TLSCert
	}

	pushInterval        int           // seconds
	pullInterval        int           // seconds
	wsReconcileInterval time.Duration // optional override for tests; default >= 10m

	usersNeedsReapply bool       // lastUsers may be an unconfirmed target; only a fresh snapshot can clear this
	configPending     bool       // A rejected or incompatible config cannot authorize new grants.
	lastUserHash      string     // hash of user list for change detection
	lastConfigHash    string     // hash of full config for change detection
	pullBackoff       apiBackoff // backoff for panel pull failures
	pushBackoff       apiBackoff // backoff for panel push failures

	// The service loop owns reportDone; the reporter closes it after restoring any unsent traffic.
	reportDone <-chan struct{}
	pullActive atomic.Bool
	// pullResults delivers async pullViaAPI results back to the main goroutine.
	pullResults chan pullResult

	wsClient              controlplane.PushClient     // Push client (nil if push is not enabled)
	wsEvents              chan controlplane.Event     // receives data events from push transport
	wsStatus              *controlplane.StatusMailbox // receives push connectivity notifications
	wsCancel              context.CancelFunc          // cancels the WS client goroutine
	wsDisconnectAt        time.Time                   // when WS last disconnected (zero if connected)
	wsResyncWanted        atomic.Uint64
	wsResyncDone          atomic.Uint64
	userMutSeq            uint64
	configMutSeq          uint64
	staleImmediateRetries int
	machineMailbox        *controlplane.NodeMailbox
	machineMailboxCh      <-chan struct{}

	// metricsMu: lastUsers, lastConfig, wsClient, wsDisconnectAt (buildMetrics vs main loop).
	metricsMu sync.RWMutex
}

// pullResult carries the outcome of an async pullViaAPI back to the main goroutine.
type kernelApplyResult struct {
	applied         bool
	rolledBackUsers []model.UserSpec // non-nil only when rollback started this exact list
}

type pullResult struct {
	configRejected  bool
	configConfirmed bool
	config          *model.NodeSpec
	users           []model.UserSpec
	configHash      string
	userHash        string
	certChanged     bool
	forced          bool
	resyncGen       uint64
	userMutSeq      uint64
	configMutSeq    uint64
	commitConfig    func()
	commitUsers     func()
}

// apiBackoff implements simple exponential backoff for API failures.
type apiBackoff struct {
	mu            sync.Mutex
	skipRemaining int
}

func (b *apiBackoff) shouldSkip() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.skipRemaining > 0 {
		b.skipRemaining--
		return true
	}
	return false
}

func (b *apiBackoff) onSuccess() {
	b.mu.Lock()
	b.skipRemaining = 0
	b.mu.Unlock()
}

func (b *apiBackoff) onFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.skipRemaining <= 0 {
		b.skipRemaining = 1
	} else if b.skipRemaining < 8 {
		b.skipRemaining *= 2
	}
}

func New(cfg *config.Config) *Service {
	var cp controlplane.ControlPlane
	if cfg.IsStandalone() {
		cp = controlplane.NewLocalControlPlane(cfg)
	} else {
		cp = controlplane.NewPanelControlPlane(cfg.Panel, cfg.WS, cfg.Kernel)
	}
	return newService(cfg, cp)
}

// ValidateXrayHandoff checks the new descriptor while the current child still owns its listener.
func (s *Service) ValidateXrayHandoff(cfg *config.Config, spec *model.NodeSpec, users []model.UserSpec) error {
	if spec.ServerPort < 1 || spec.ServerPort > 65535 {
		return fmt.Errorf("invalid server port %d", spec.ServerPort)
	}
	// These are the inbound builders that actually apply XHTTP stream settings.
	switch spec.Protocol {
	case "vless", "vmess", "trojan":
	default:
		return fmt.Errorf("XHTTP is not available for %q", spec.Protocol)
	}
	certCfg := cfg.Cert
	if spec.CertConfig != nil {
		certCfg = *spec.CertConfig
		certCfg.CertDir = cfg.Cert.CertDir
	}
	tls, configured, err := cert.PreviewMaterial(certCfg)
	if err != nil {
		return err
	}
	if spec.TLS == 1 && !configured {
		return fmt.Errorf("TLS inbound requires a certificate source")
	}
	// Runtime validation also considers automatic certificate sources.
	candidate := *spec
	candidate.CertConfig = &certCfg
	if err := validateNodeRuntime(cfg, xray.New(cfg.Kernel).Protocols(), &candidate, tls); err != nil {
		return err
	}
	if err := xray.PreflightConfig(cfg.Kernel, spec, users, tls); err != nil {
		return err
	}
	s.metricsMu.RLock()
	previous := s.lastConfig
	s.metricsMu.RUnlock()
	if previous != nil && spec.ServerPort != previous.ServerPort && len(users) > 0 {
		listener, err := net.Listen("tcp", net.JoinHostPort(spec.ListenIP, strconv.Itoa(spec.ServerPort)))
		if err != nil {
			return fmt.Errorf("new listener is unavailable: %w", err)
		}
		return listener.Close()
	}
	return nil
}

// StoppedConfig returns the last descriptor after Run and its shutdown cleanup have finished.
// Rollback must obtain its own fresh list of authorized users.
func (s *Service) StoppedConfig() *model.NodeSpec { return s.lastConfig }

// StoppedUsers is read by the orchestrator after the child has joined.
func (s *Service) StoppedUsers() []model.UserSpec { return cloneUsersNonNil(s.appliedState.Users) }

// NewWithControlPlane creates a Service with an externally-provided
// ControlPlane. Used by the machine orchestrator to inject a
// MachinePanelControlPlane with WS mux routing.
func NewWithControlPlane(cfg *config.Config, cp controlplane.ControlPlane) *Service {
	return newService(cfg, cp)
}

func newService(cfg *config.Config, cp controlplane.ControlPlane) *Service {
	certMgr := cert.NewManager(cfg.Cert)

	var k kernel.Kernel
	switch cfg.Kernel.Type {
	case "singbox":
		k = singbox.New(cfg.Kernel)
	case "xray":
		k = xray.New(cfg.Kernel)
	default:
		nlog.Core().Warn("unsupported kernel type, defaulting to sing-box", "type", cfg.Kernel.Type)
		k = singbox.New(cfg.Kernel)
	}

	l := limiter.New()
	st := limiter.NewSpeedTracker(l)

	return &Service{
		cfg:          cfg,
		source:       cp,
		sink:         cp,
		kernel:       k,
		portHopping:  porthopping.New(cfg.Kernel.ConfigDir),
		tracker:      tracker.New(),
		limiter:      l,
		speedTracker: st,
		cert:         certMgr,
		baseCertCfg:  cloneCertConfig(cfg.Cert),
		baseCertInit: true,
		wsEvents:     make(chan controlplane.Event, 16),
		wsStatus:     controlplane.NewStatusMailbox(),
		pullResults:  make(chan pullResult, 1),
	}
}

func (s *Service) Run(ctx context.Context) error {
	if owner, ok := s.source.(interface{ CloseIdleConnections() }); ok {
		defer owner.CloseIdleConnections()
	}
	if x, ok := s.kernel.(*xray.Xray); ok {
		x.SetResourceContext(ctx)
	}
	// Start cert manager (handles auto-TLS or manual cert verification)
	if err := s.cert.Start(ctx); err != nil {
		return fmt.Errorf("cert manager: %w", err)
	}
	defer s.cert.Stop()

	// Handshake: get WS config + initial data in one call
	if err := s.initialSetup(ctx); err != nil {
		s.stopKernel()
		return fmt.Errorf("initial setup: %w", err)
	}
	// Stop accepting traffic before the final report can block on HTTP.
	defer s.pushReportSync(FinalReportTimeout)
	defer s.stopKernel()

	// Set up tickers
	trackTicker := time.NewTicker(time.Duration(s.cfg.Node.TrackInterval) * time.Second)
	pushInterval := time.Duration(math.Max(float64(s.pushInterval), 5)) * time.Second
	pullInterval := time.Duration(s.pullInterval) * time.Second
	reportTicker := time.NewTicker(pushInterval)
	pullTicker := time.NewTicker(pullInterval)
	deviceReportTicker := time.NewTicker(time.Duration(s.cfg.Node.DeviceReportInterval) * time.Second)

	// WS discovery: when in REST-only mode, periodically re-handshake to check
	// if WS has been enabled. When WS is disconnected for too long, re-check
	// if it's still available.
	wsDiscoveryTicker := time.NewTicker(time.Duration(s.cfg.WS.DiscoveryInterval) * time.Second)

	reconcileInterval := s.wsReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = 5 * pullInterval
		if reconcileInterval < 10*time.Minute {
			reconcileInterval = 10 * time.Minute
		}
	}
	wsReconcileTicker := time.NewTicker(reconcileInterval)

	defer trackTicker.Stop()
	defer reportTicker.Stop()
	defer pullTicker.Stop()
	defer deviceReportTicker.Stop()
	defer wsDiscoveryTicker.Stop()
	defer wsReconcileTicker.Stop()

	s.startWSClient(ctx)
	var discovery <-chan pushDiscoveryResult

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-trackTicker.C:
			s.trackAndEnforce(ctx)

		case <-reportTicker.C:
			s.pushReportAsync()

		case <-deviceReportTicker.C:
			s.reportDevices()

		case <-s.cert.RenewedCh():
			s.handleCertRenewal(ctx)

		case <-pullTicker.C:
			s.staleImmediateRetries = 0
			if s.cert.HasPendingRenewal() {
				s.handleCertRenewal(ctx)
			}
			// When WebSocket is connected, skip frequent REST polling unless a
			// forced reconciliation is still pending (e.g. after backoff or apply failure).
			if s.wsClient != nil && s.wsClient.IsConnected() {
				if s.hasPendingForceResync() {
					nlog.Core().Debug("retrying pending forced REST reconciliation")
					s.pullViaAPIWithOptions(ctx, true)
				}
				continue
			}
			nlog.Core().Debug("polling from API (ws not connected)")
			s.pullViaAPIAsync(ctx)

		case <-wsReconcileTicker.C:
			s.staleImmediateRetries = 0
			if s.source != nil && s.source.SupportsPolling() && s.wsClient != nil && s.wsClient.IsConnected() {
				nlog.Core().Debug("ws periodic reconciliation pull")
				s.pullViaAPIWithOptions(ctx, true)
			}

		case result := <-s.pullResults:
			s.applyPullResult(ctx, result)

		case <-wsDiscoveryTicker.C:
			if s.source.SupportsDiscovery() && discovery == nil {
				completed := make(chan pushDiscoveryResult, 1)
				discovery = completed
				go func() { completed <- s.discoverPush(ctx) }()
			}
		case result := <-discovery:
			discovery = nil
			s.applyPushDiscovery(ctx, result)

		case <-s.wsStatus.NotifyCh():
			s.drainWSStatus(ctx)

		case <-s.machineMailboxCh:
			s.drainMachineMailbox(ctx)

		case event := <-s.wsEvents:
			s.drainWSStatus(ctx)
			s.handleWSEvent(ctx, event)
		}
	}
}

func (s *Service) initialSetup(ctx context.Context) error {
	// Register speed limit lookup with kernel unconditionally (before push/poll branch).
	s.kernel.SetSpeedLimitFunc(s.speedTracker.GetLimiter)
	s.kernel.SetDeviceLimitFunc(s.limiter.GetDeviceLimitByUUID)
	s.kernel.SetUserPolicy(&s.userPolicy)

	bootstrap, err := s.source.Initial(ctx, s.wsMetrics, s.wsEvents, s.wsStatus)
	if err != nil {
		return err
	}

	if s.cfg.Node.PushInterval == 0 && bootstrap.PushInterval > 0 {
		s.pushInterval = bootstrap.PushInterval
	} else {
		s.pushInterval = s.cfg.Node.PushInterval
	}
	if s.pushInterval == 0 {
		s.pushInterval = 60
	}

	if s.cfg.Node.PullInterval == 0 && bootstrap.PullInterval > 0 {
		s.pullInterval = bootstrap.PullInterval
	} else {
		s.pullInterval = s.cfg.Node.PullInterval
	}
	if s.pullInterval == 0 {
		s.pullInterval = 60
	}

	if bootstrap.Push != nil {
		s.wsClient = bootstrap.Push
	}
	s.machineMailbox = bootstrap.Mailbox
	if bootstrap.ConfigPending {
		s.holdConfigGrants()
	}
	if s.machineMailbox != nil {
		s.machineMailboxCh = s.machineMailbox.NotifyCh()
	}
	if bootstrap.Config == nil {
		if bootstrap.Push != nil {
			// In machine mode a shared WS client may be available before the first
			// per-node snapshot arrives. In that case we wait for subsequent WS/REST
			// updates instead of failing startup.
			return nil
		}
		return fmt.Errorf("initial config is nil")
	}
	if err := validateCandidateFields(s.cfg, s.kernel.Protocols(), bootstrap.Config); err != nil {
		return err
	}

	s.metricsMu.Lock()
	s.lastConfig = bootstrap.Config
	s.metricsMu.Unlock()
	s.lastConfigHash = computeConfigHash(bootstrap.Config)
	userConflict := false
	if s.machineMailbox != nil {
		bootstrap.Users, userConflict = s.machineMailbox.ArbitrateUsers(bootstrap.Users)
	}
	s.stageUserState(bootstrap.Users, "")

	nlog.Core().Info("initial snapshot ready",
		"protocol", bootstrap.Config.Protocol,
		"port", bootstrap.Config.ServerPort,
		"users", len(bootstrap.Users),
	)

	if _, err := s.applyRemoteOverrides(ctx, bootstrap.Config); err != nil {
		return err
	}
	if err := validateTLSRequirements(bootstrap.Config, s.cert.TLSCert(), cfgKernelType(s.cfg)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(bootstrap.Users) == 0 {
		nlog.Core().Warn("no users, kernel will not start until users are available")
	} else if !s.startKernel(bootstrap.Config, bootstrap.Users) {
		return fmt.Errorf("start kernel")
	}
	s.cert.Commit()
	if bootstrap.CommitConfig != nil {
		bootstrap.CommitConfig()
	}
	if !userConflict && bootstrap.CommitUsers != nil {
		bootstrap.CommitUsers()
	}
	s.markMailboxReadyAndDrain(ctx)
	if userConflict {
		s.pullViaAPIWithOptions(ctx, true)
	}
	return nil
}

// applyRemoteOverrides updates service-level settings (log level, cert config)
// from the panel's NodeConfig. Returns true if cert paths changed (kernel restart needed).
func (s *Service) applyRemoteOverrides(ctx context.Context, nc *model.NodeSpec) (bool, error) {
	if nc == nil {
		return false, nil
	}
	if !s.baseCertInit {
		s.baseCertCfg = cloneCertConfig(s.cfg.Cert)
		s.baseCertInit = true
	}

	prevLogLevel := s.cfg.Kernel.LogLevel
	// Dynamic Log Level (Kernel)
	if nc.KernelLogLevel != "" && nc.KernelLogLevel != s.cfg.Kernel.LogLevel {
		nlog.Core().Info("cert: kernel log level override", "old", s.cfg.Kernel.LogLevel, "new", nc.KernelLogLevel)
		s.cfg.Kernel.LogLevel = nc.KernelLogLevel
	}

	// Certificate configuration from panel (panel-first: takes precedence over local config)
	if nc.CertConfig != nil {
		changed, err := s.applyNodeCert(ctx, nc.CertConfig)
		if err != nil {
			s.cfg.Kernel.LogLevel = prevLogLevel
			return false, err
		}
		s.panelCertOverride = true
		return changed, nil
	}

	// Panel cleared its certificate override (e.g., switched back to cert_mode=none):
	// restore the local node's initial cert configuration.
	if s.panelCertOverride {
		restored := cloneCertConfig(s.baseCertCfg)
		if nc.AutoTLS {
			restored.AutoTLS = true
		}
		if nc.Domain != "" && restored.Domain == "" {
			restored.Domain = nc.Domain
		}
		changed, err := s.applyNodeCert(ctx, &restored)
		if err != nil {
			s.cfg.Kernel.LogLevel = prevLogLevel
			return false, err
		}
		s.panelCertOverride = false
		return changed, nil
	}

	// Legacy fields (deprecated: prefer cert_config)
	if nc.AutoTLS != s.cfg.Cert.AutoTLS {
		nlog.Core().Info("cert: auto_tls policy changed (deprecated field)", "new", nc.AutoTLS)
		s.cfg.Cert.AutoTLS = nc.AutoTLS
	}
	if nc.Domain != "" && nc.Domain != s.cfg.Cert.Domain {
		s.cfg.Cert.Domain = nc.Domain
	}

	return false, nil
}

// applyNodeCert converts a panel CertConfig into the local config format and
// reconfigures the cert manager. Reports whether cert paths changed.
func (s *Service) applyNodeCert(ctx context.Context, newCfg *config.CertConfig) (bool, error) {
	if newCfg == nil {
		return false, nil
	}
	cfgCopy := *newCfg
	cfgCopy.CertDir = s.cfg.Cert.CertDir
	cfgCopy.LegacyCertDir = s.cfg.Cert.LegacyCertDir

	changed, err := s.cert.Reconfigure(ctx, cfgCopy)
	if err != nil {
		nlog.Core().Error("failed to apply runtime cert config", "mode", cfgCopy.CertMode, "error", err)
		return false, err
	}
	s.cfg.Cert = cfgCopy
	if changed {
		msg := fmt.Sprintf("cert: material updated, has_cert=%v", s.cert.HasCert())
		if s.nodeLog != nil {
			s.nodeLog.Info(msg)
		} else {
			nlog.Core().Info(msg)
		}
	}
	return changed, nil
}

// startWSClient starts the push client goroutine if a client is configured.
func (s *Service) startWSClient(ctx context.Context) {
	if s.wsClient == nil {
		return
	}
	wsCtx, wsCancel := context.WithCancel(ctx)
	s.wsCancel = wsCancel
	go s.wsClient.Run(wsCtx)
}

func (s *Service) markMailboxReadyAndDrain(ctx context.Context) {
	if s.machineMailbox == nil {
		return
	}
	// Seed mailbox with bootstrap state so delta events can be applied
	// incrementally instead of always triggering REST reconciliation.
	s.metricsMu.RLock()
	users := s.lastUsers
	config := s.lastConfig
	s.metricsMu.RUnlock()
	s.machineMailbox.SeedBaseline(users, config)
	s.machineMailbox.MarkReady()
	s.drainMachineMailbox(ctx)
}

func (s *Service) drainMachineMailbox(ctx context.Context) {
	if s.machineMailbox == nil {
		return
	}
	state := s.machineMailbox.DrainIfReady()
	if state.ConfigPending || (state.HasConfig && state.HasUsers && computeConfigHash(state.Config) != s.lastConfigHash) {
		s.configMutSeq++
		s.holdConfigGrants()
	}
	if state.HasUsers {
		users := state.Users
		if users == nil {
			users = []model.UserSpec{}
		}
		s.handleWSEvent(ctx, controlplane.Event{Type: controlplane.EventSyncUsers, Users: users})
	}
	// A config restart must see withdrawals from this same mailbox batch.
	if state.HasConfig {
		s.handleWSEvent(ctx, controlplane.Event{Type: controlplane.EventSyncConfig, Config: state.Config})
	}
	if state.HasDevices {
		s.handleWSEvent(ctx, controlplane.Event{Type: controlplane.EventSyncDevices, DeviceUsers: state.DeviceUsers})
	}
	// A pending notification or a config processed while grants are held needs a
	// fresh config/roster pair, even when the recovered config hash is unchanged.
	if state.NeedsReconcile || (state.HasConfig && s.configPending) {
		s.requestWSResync(ctx, "machine_mailbox_reconcile")
	}
}

func (s *Service) hasPendingForceResync() bool {
	return s.wsResyncWanted.Load() > s.wsResyncDone.Load()
}

func (s *Service) requestWSResync(ctx context.Context, reason string) {
	if s.source == nil || !s.source.SupportsPolling() {
		return
	}
	s.wsResyncWanted.Add(1)
	if s.nodeLog != nil {
		s.nodeLog.Warn("ws state may be stale, scheduling REST reconciliation", "reason", reason)
	} else {
		nlog.Core().Warn("ws state may be stale, scheduling REST reconciliation", "reason", reason)
	}
	s.pullViaAPIWithOptions(ctx, true)
}

func (s *Service) wsMetrics() map[string]interface{} {
	status := monitor.Collect()
	return s.buildMetrics(status, s.kernel.IsRunning())
}

func (s *Service) drainWSStatus(ctx context.Context) {
	if state, ok := s.wsStatus.Drain(); ok {
		s.handleWSStatus(ctx, state)
	}
}

// handleWSStatus reacts to WS connectivity changes.

// - On disconnect: record timestamp, immediately REST poll.
// - On reconnect: clear disconnect timestamp, REST poll to catch missed events.
func (s *Service) handleWSStatus(ctx context.Context, status controlplane.StatusChange) {
	if status.ConfigPending {
		s.holdConfigGrants()
	}
	if status.SawDisconnect || !status.Connected {
		s.kernel.ClearGlobalDevices()
	}
	if status.Connected {
		s.metricsMu.Lock()
		s.wsDisconnectAt = time.Time{}
		s.metricsMu.Unlock()
		// Use nodeLog if available, otherwise core
		if s.nodeLog != nil {
			s.nodeLog.Info("ws connected")
		} else {
			nlog.Core().Info("ws connected")
		}
	} else {
		s.metricsMu.Lock()
		if s.wsDisconnectAt.IsZero() {
			s.wsDisconnectAt = time.Now()
		}
		s.metricsMu.Unlock()
		if s.nodeLog != nil {
			s.nodeLog.Info("ws disconnected")
		} else {
			nlog.Core().Info("ws disconnected")
		}
	}
	if status.Connected || status.NeedsResync {
		// A response already in flight predates the reconnect or dropped frame.
		s.configMutSeq++
		s.userMutSeq++
		s.requestWSResync(ctx, "ws_reconnect_or_drop")
	} else {
		s.pullViaAPIAsync(ctx)
	}
}

// Each discovered endpoint owns its event channels. Retiring that pair also drops
// buffered updates and late disconnects from the previous endpoint.
type pushDiscoveryResult struct {
	client   controlplane.PushClient
	events   chan controlplane.Event
	statuses *controlplane.StatusMailbox
	err      error
}

func (s *Service) discoverPush(ctx context.Context) pushDiscoveryResult {
	r := pushDiscoveryResult{events: make(chan controlplane.Event, cap(s.wsEvents)), statuses: controlplane.NewStatusMailbox()}
	r.client, r.err = s.source.Discover(ctx, s.wsMetrics, r.events, r.statuses)
	return r
}

func (s *Service) applyPushDiscovery(ctx context.Context, result pushDiscoveryResult) {
	client, err := result.client, result.err
	if err != nil || ctx.Err() != nil {
		nlog.Core().Debug("push discovery failed", "error", err)
		return
	}
	if client == nil && s.wsClient == nil {
		return
	}
	if old, ok := s.wsClient.(interface{ Endpoint() string }); ok {
		if next, ok := client.(interface{ Endpoint() string }); ok && old.Endpoint() == next.Endpoint() {
			return
		}
	}
	if s.wsCancel != nil {
		s.wsCancel()
		s.wsCancel = nil
	}
	s.wsEvents, s.wsStatus = result.events, result.statuses
	s.metricsMu.Lock()
	s.wsClient = client
	s.wsDisconnectAt = time.Time{}
	s.metricsMu.Unlock()
	s.configMutSeq++
	s.userMutSeq++
	s.startWSClient(ctx)
	s.requestWSResync(ctx, "ws_endpoint_changed")
}

// handleWSEvent processes data events received via WebSocket
func (s *Service) handleWSEvent(ctx context.Context, event controlplane.Event) {
	switch event.Type {
	case controlplane.EventConfigPending:
		s.configMutSeq++
		s.holdConfigGrants()
	case controlplane.EventSyncConfig:
		if event.Config == nil {
			return
		}
		newConfigHash := computeConfigHash(event.Config)
		s.configMutSeq++
		if err := validateCandidateFields(s.cfg, s.kernel.Protocols(), event.Config); err != nil {
			s.holdConfigGrants()
			nlog.Core().Warn("ws config validation failed, ignoring update", "error", err)
			return
		}
		// Advance generation even on hash equality so ABA WS coalescing (C0 -> C1 -> C0)
		// still invalidates any in-flight REST snapshot carrying intermediate C1.
		if newConfigHash == s.lastConfigHash {
			return
		}
		// Initialize nodeLog on first config
		if s.nodeLog == nil {
			s.nodeLog = nlog.ForNode(event.Config.Protocol, event.Config.ServerPort)
		}
		s.nodeLog.Info(fmt.Sprintf("config updated, %d users", len(event.Users)))
		s.metricsMu.RLock()
		prevConfig := s.lastConfig
		s.metricsMu.RUnlock()
		prevConfigHash := s.lastConfigHash
		prevCertCfg := cloneCertConfig(s.cfg.Cert)
		prevLogLevel := s.cfg.Kernel.LogLevel
		prevPanelCertOverride := s.panelCertOverride

		if _, err := s.applyRemoteOverrides(ctx, event.Config); err != nil {
			s.holdConfigGrants()
			nlog.Core().Warn("ws remote overrides failed, keeping existing config", "error", err)
			s.restoreCertOverrideRollback(prevCertCfg, prevLogLevel, prevPanelCertOverride)
			return
		}
		if err := validateTLSRequirements(event.Config, s.cert.TLSCert(), cfgKernelType(s.cfg)); err != nil {
			s.holdConfigGrants()
			nlog.Core().Warn("ws post-override TLS validation failed, keeping existing config", "error", err)
			s.restoreCertOverrideRollback(prevCertCfg, prevLogLevel, prevPanelCertOverride)
			return
		}

		s.metricsMu.Lock()
		s.lastConfig = event.Config
		s.metricsMu.Unlock()
		s.lastConfigHash = newConfigHash
		if !s.applyConfigWithUsers(s.safeUsersForRestart()).applied {
			s.holdConfigGrants()
			s.metricsMu.Lock()
			s.lastConfig = prevConfig
			s.metricsMu.Unlock()
			s.lastConfigHash = prevConfigHash
			s.restoreCertOverrideRollback(prevCertCfg, prevLogLevel, prevPanelCertOverride)
		} else {
			s.cert.Commit()
			if s.source != nil {
				if resetter, ok := s.source.(interface{ ResetConfigETag() }); ok {
					resetter.ResetConfigETag()
				}
			}
		}

	case controlplane.EventSyncUsers:
		if event.Users == nil {
			return
		}
		// Advance generation before hash early-return so ABA WS coalescing ([A] -> [A,B] -> [A])
		// still rejects an in-flight REST snapshot carrying [A,B].
		s.userMutSeq++
		newHash := computeUserHash(event.Users)
		if s.usersNeedsReapply || s.configPending {
			s.restrictPendingUsers(event.Users)
			return
		}
		if newHash == s.lastUserHash {
			return
		}
		if s.nodeLog != nil {
			s.nodeLog.Info(fmt.Sprintf("users updated, %d users", len(event.Users)))
		}
		if s.applyUserUpdate(ctx, event.Users, newHash) {
			if s.source != nil {
				if resetter, ok := s.source.(interface{ ResetUserETag() }); ok {
					resetter.ResetUserETag()
				}
			}
		} else {
			s.pullViaAPIWithOptions(ctx, true)
		}

	case controlplane.EventSyncUserDelta:
		if len(event.DeltaUsers) == 0 {
			return
		}
		s.userMutSeq++
		if s.nodeLog != nil {
			s.nodeLog.Info(fmt.Sprintf("users delta: %s, %d users", event.DeltaAction, len(event.DeltaUsers)))
		}
		s.applyUserDelta(ctx, event.DeltaAction, event.DeltaUsers)
		if s.source != nil {
			if resetter, ok := s.source.(interface{ ResetUserETag() }); ok {
				resetter.ResetUserETag()
			}
		}

	case controlplane.EventSyncDevices:
		// Sync global device state
		if event.DeviceUsers != nil {
			s.kernel.UpdateGlobalDevices(event.DeviceUsers)
		}

	default:
		nlog.Core().Debug(fmt.Sprintf("unknown ws event: %v", event.Type))
	}
}

func (s *Service) handleCertRenewal(ctx context.Context) {
	tls, generation, pending := s.cert.PendingRenewalSnapshot()
	if !pending {
		return
	}

	s.metricsMu.RLock()
	cfg := s.lastConfig
	users := s.safeUsersForRestart()
	s.metricsMu.RUnlock()

	if cfg == nil || len(users) == 0 {
		s.cert.AckRenewedThrough(generation)
		return
	}

	nlog.Core().Info("certificate renewed, reloading kernel")
	result := s.applyKernelWithTLS(cfg, users, s.kernel.IsRunning(), tls)
	if result.applied {
		s.cert.AckRenewedThrough(generation)
	}
}

// pullViaAPIAsync fetches config/users from the panel API in a background
// goroutine and sends the result to pullResults for the main goroutine to apply.
func (s *Service) pullViaAPIAsync(ctx context.Context) {
	s.pullViaAPIWithOptions(ctx, false)
}

func (s *Service) pullViaAPIWithOptions(ctx context.Context, forceFull bool) {
	if s.source == nil || !s.source.SupportsPolling() {
		return
	}
	if forceFull && !s.hasPendingForceResync() {
		s.wsResyncWanted.Add(1)
	}
	force := forceFull || s.hasPendingForceResync()

	if !s.pullActive.CompareAndSwap(false, true) {
		nlog.Core().Debug("pull already in progress, skipping")
		return
	}
	if s.pullBackoff.shouldSkip() {
		nlog.Core().Debug("skipping pull due to backoff")
		s.pullActive.Store(false)
		return
	}

	currentConfigHash := s.lastConfigHash
	startUserSeq := s.userMutSeq
	startConfigSeq := s.configMutSeq
	resyncGen := uint64(0)
	if force {
		resyncGen = s.wsResyncWanted.Load()
	}

	go func() {
		if force {
			if resetter, ok := s.source.(interface{ ResetConfigETag() }); ok {
				resetter.ResetConfigETag()
			}
			if resetter, ok := s.source.(interface{ ResetUserETag() }); ok {
				resetter.ResetUserETag()
			}
		}
		snapshot, err := s.source.Poll(ctx)
		if err != nil {
			nlog.Core().Error("poll control plane failed", "error", err)
			s.pullBackoff.onFailure()
			if snapshot.Users == nil && !snapshot.ConfigPending {
				s.pullActive.Store(false)
				return
			}
		} else {
			s.pullBackoff.onSuccess()
		}

		result := pullResult{
			configRejected:  err != nil || snapshot.ConfigPending,
			configConfirmed: snapshot.Config != nil,
			commitConfig:    snapshot.CommitConfig,
			commitUsers:     snapshot.CommitUsers,
			forced:          force,
			resyncGen:       resyncGen,
			userMutSeq:      startUserSeq,
			configMutSeq:    startConfigSeq,
		}
		if snapshot.Config != nil {
			result.config = snapshot.Config
			result.configHash = computeConfigHash(snapshot.Config)
			if result.configHash == currentConfigHash {
				result.config = nil
			}
		}
		if snapshot.Users != nil {
			result.users = snapshot.Users
			result.userHash = computeUserHash(snapshot.Users)
		}

		if s.pullResults == nil {
			s.pullActive.Store(false)
			return
		}
		select {
		case s.pullResults <- result:
		case <-ctx.Done():
			s.pullActive.Store(false)
		}
	}()
}

// applyPullResult processes the result of an async pullViaAPI on the main goroutine.
func (s *Service) applyPullResult(ctx context.Context, result pullResult) {
	if s.machineMailbox != nil && s.machineMailbox.HasPendingConfig() {
		s.drainMachineMailbox(ctx)
	}
	// The event loop can select a completed poll ahead of a queued WS wakeup.
	// Observe the newest status while the previous poll still owns pullActive.
	s.drainWSStatus(ctx)
	s.pullActive.Store(false)
	configStale := result.configMutSeq != s.configMutSeq
	usersStale := result.userMutSeq != s.userMutSeq
	if configStale {
		nlog.Core().Debug("discarding stale REST config superseded by WS update")
		result.config = nil
		result.commitConfig = nil
	}
	if usersStale {
		nlog.Core().Debug("discarding stale REST users superseded by WS update")
		result.users = nil
		result.commitUsers = nil
	}

	configChanged := false
	configOK := !result.configRejected && (!s.configPending || (!configStale && (result.configConfirmed || result.config != nil)))
	usersOK := !s.usersNeedsReapply

	s.metricsMu.RLock()
	prevConfig := s.lastConfig
	s.metricsMu.RUnlock()
	prevConfigHash := s.lastConfigHash
	prevCertCfg := cloneCertConfig(s.cfg.Cert)
	prevLogLevel := s.cfg.Kernel.LogLevel
	prevPanelCertOverride := s.panelCertOverride

	if result.certChanged {
		nlog.Core().Info("certificate renewed, kernel restart needed")
		configChanged = true
	}

	var appliedConfig *model.NodeSpec

	if result.config != nil {
		if err := validateCandidateFields(s.cfg, s.kernel.Protocols(), result.config); err != nil {
			nlog.Core().Warn("runtime config validation failed", "error", err)
			result.config = nil
			configOK = false
		} else {
			if _, err := s.applyRemoteOverrides(ctx, result.config); err != nil {
				nlog.Core().Warn("remote overrides apply failed, keeping existing config", "error", err)
				s.restoreCertOverrideRollback(prevCertCfg, prevLogLevel, prevPanelCertOverride)
				result.config = nil
				configOK = false
			} else if err := validateTLSRequirements(result.config, s.cert.TLSCert(), cfgKernelType(s.cfg)); err != nil {
				nlog.Core().Warn("post-override TLS validation failed, keeping existing config", "error", err)
				s.restoreCertOverrideRollback(prevCertCfg, prevLogLevel, prevPanelCertOverride)
				result.config = nil
				configOK = false
			} else {
				configChanged = true
				// Initialize or update node logger
				if s.nodeLog == nil {
					s.nodeLog = nlog.ForNode(result.config.Protocol, result.config.ServerPort)
				}
				s.nodeLog.Info(fmt.Sprintf("config updated, %d users", len(s.lastUsers)))
				s.metricsMu.Lock()
				s.lastConfig = result.config
				s.metricsMu.Unlock()
				s.lastConfigHash = result.configHash
				appliedConfig = result.config
			}
		}
	}

	// A rejected candidate cannot authorize new credentials or relax limits.
	// Publish the restricted list to the mailbox as well as the running kernel.
	if !configOK && !configStale {
		s.holdConfigGrants()
	}
	if (!configOK || (s.configPending && configStale)) && result.users != nil {
		result.users = model.IntersectUsers(s.appliedState.Users, result.users)
		result.userHash = computeUserHash(result.users)
		result.commitUsers = nil
	}
	mailboxConflict := false
	if result.users != nil && s.machineMailbox != nil {
		result.users, mailboxConflict = s.machineMailbox.ArbitrateUsers(result.users)
		if mailboxConflict {
			result.userHash = computeUserHash(result.users)
		}
	}

	// A no-body 304 says nothing about an uncommitted target. Keep potential
	// new grants out of config reloads until a non-stale full response arrives.
	target := s.safeUsersForRestart()
	usersChanged := result.users != nil && (result.userHash != s.lastUserHash || s.usersNeedsReapply ||
		(s.configPending && computeUserHash(s.appliedState.Users) != result.userHash))
	if result.users != nil {
		if s.usersNeedsReapply && mailboxConflict {
			s.restrictPendingUsers(result.users)
			target = s.safeUsersForRestart()
			usersChanged = false
		} else {
			target = result.users
			usersOK = true
			if usersChanged && configChanged {
				// Both kernels can restart their cached old list on a failed
				// config reload. Enforce withdrawals on that list first.
				safe := s.stageUserState(target, result.userHash)
				if s.kernel.IsRunning() && (len(safe) == 0 || s.usersNeedsReapply) {
					s.stopKernelInstance()
				} else if s.kernel.IsRunning() && computeUserHash(safe) != computeUserHash(s.appliedState.Users) {
					if _, _, err := s.kernel.UpdateUsers(safe); err != nil {
						s.stopKernelInstance()
					}
				}
				s.appliedState.Users = safe
			} else if usersChanged {
				usersOK = s.applyUserUpdate(ctx, target, result.userHash)
			}
		}
	}

	if configChanged {
		change := s.applyConfigWithUsers(target)
		if !change.applied {
			s.holdConfigGrants()
			configOK = false
			appliedConfig = nil
			s.metricsMu.Lock()
			s.lastConfig = prevConfig
			s.metricsMu.Unlock()
			s.lastConfigHash = prevConfigHash
			s.restoreCertOverrideRollback(prevCertCfg, prevLogLevel, prevPanelCertOverride)
			if usersChanged {
				target = model.IntersectUsers(s.appliedState.Users, target)
				result.userHash = computeUserHash(target)
				result.commitUsers = nil
				if s.machineMailbox != nil {
					target, mailboxConflict = s.machineMailbox.ArbitrateUsers(target)
					result.userHash = computeUserHash(target)
				}
				if change.rolledBackUsers != nil {
					s.restoreUserState(change.rolledBackUsers, computeUserHash(change.rolledBackUsers))
					usersOK = result.userHash == s.lastUserHash || s.applyUserUpdate(ctx, target, result.userHash)
				} else if s.kernel.IsRunning() {
					// A kernel-managed recovery does not report its exact user list.
					// Do not trust its cached diffs to confirm the new target.
					usersOK = s.rebuildUsers(target)
				} else {
					s.recordUserApplyFailure(target, nil)
					usersOK = false
				}
			}
		} else if result.config != nil {
			s.cert.Commit()
		}
	}
	if !configChanged && !configStale && !usersStale && configOK && usersOK &&
		!s.kernel.IsRunning() && s.lastConfig != nil && len(target) > 0 {
		usersOK = s.rebuildUsers(target)
	}

	if result.users != nil && len(result.users) > 0 && !s.kernel.IsRunning() {
		// A list can arrive before the first usable config. Keep it staged, but
		// do not accept its ETag before any listener can serve that list.
		usersOK = false
	}
	if result.users != nil && usersOK && !mailboxConflict {
		s.usersNeedsReapply = false
		if !configStale && configOK {
			s.configPending = false
		}
	}
	if !configStale && configOK && result.commitConfig != nil {
		result.commitConfig()
	}
	if result.users != nil && usersOK && !s.configPending && !mailboxConflict && result.commitUsers != nil {
		result.commitUsers()
	}
	if s.machineMailbox != nil && appliedConfig != nil {
		s.machineMailbox.SyncConfigBaseline(appliedConfig)
	}
	if result.forced && result.users != nil && !mailboxConflict && !configStale && !usersStale && configOK && usersOK {
		if result.resyncGen > s.wsResyncDone.Load() {
			s.wsResyncDone.Store(result.resyncGen)
		}
	}

	immediateRetry := false
	if configStale || usersStale || mailboxConflict {
		if !s.hasPendingForceResync() {
			s.wsResyncWanted.Add(1)
		}
		if s.staleImmediateRetries < maxImmediateStaleRetries {
			s.staleImmediateRetries++
			immediateRetry = true
		} else {
			nlog.Core().Debug("deferring further forced resync retry to next poll tick after repeated WS overlap")
		}
	} else {
		s.staleImmediateRetries = 0
		if !result.forced && s.hasPendingForceResync() {
			immediateRetry = true
		} else if result.forced && result.users != nil && configOK && usersOK && s.hasPendingForceResync() {
			immediateRetry = true
		}
	}

	if immediateRetry {
		s.pullViaAPIWithOptions(ctx, true)
	}
}

// ─── User state helpers ─────────────────────────────────────────────────────

func (s *Service) restoreCertOverrideRollback(prevCertCfg config.CertConfig, prevLogLevel string, prevOverride bool) {
	s.cfg.Cert = prevCertCfg
	s.cfg.Kernel.LogLevel = prevLogLevel
	s.panelCertOverride = prevOverride
	s.cert.Rollback()

}

func cloneCertConfig(cfg config.CertConfig) config.CertConfig {
	cloned := cfg
	if cfg.DNSEnv != nil {
		envCopy := make(map[string]string, len(cfg.DNSEnv))
		for k, v := range cfg.DNSEnv {
			envCopy[k] = v
		}
		cloned.DNSEnv = envCopy
	}
	return cloned
}

func intersectAppliedUsers(applied, current []model.UserSpec) []model.UserSpec {
	common := model.IntersectUsers(applied, current)
	if len(common) == 0 {
		return nil
	}
	return common
}

func cloneUsersNonNil(users []model.UserSpec) []model.UserSpec {
	out := make([]model.UserSpec, len(users))
	copy(out, users)
	return out
}

func cloneTLSCert(tls kernel.TLSCert) kernel.TLSCert {
	if !tls.HasCert() {
		return kernel.TLSCert{}
	}
	return kernel.TLSCert{
		CertPEM: append([]byte(nil), tls.CertPEM...),
		KeyPEM:  append([]byte(nil), tls.KeyPEM...),
	}
}

func (s *Service) updateUserState(users []model.UserSpec) {
	s.restoreUserState(users, computeUserHash(users))
}

// Prepare kernel-visible buckets for new users while both dispatchers admit
// only the confirmed intersection. Existing buckets retain the tighter rate.
func (s *Service) stageUserState(target []model.UserSpec, hash string) []model.UserSpec {
	safe := intersectAppliedUsers(s.appliedState.Users, target)
	s.userPolicy.Publish(safe)
	// Buckets are shared by numeric ID. A UUID rotation still has to keep
	// the old connection's cap until its replacement is confirmed.
	oldSpeed := make(map[int]int, len(s.appliedState.Users))
	for _, u := range s.appliedState.Users {
		if u.SpeedLimit > 0 {
			oldSpeed[u.ID] = u.SpeedLimit
		}
	}
	prepared := cloneUsersNonNil(target)
	for i, u := range prepared {
		if old := oldSpeed[u.ID]; old > 0 && (u.SpeedLimit <= 0 || old < u.SpeedLimit) {
			prepared[i].SpeedLimit = old
		}
	}
	s.setUserLimits(prepared)
	s.rememberUsers(target, hash)
	return safe
}

func (s *Service) setUserLimits(users []model.UserSpec) {
	s.limiter.UpdateUsers(users)
	s.speedTracker.UpdateBuckets()
}

func (s *Service) publishUserPolicy(users []model.UserSpec) {
	s.setUserLimits(users)
	s.userPolicy.Publish(users)
}

func (s *Service) restoreUserState(users []model.UserSpec, hash string) {
	s.publishUserPolicy(users)
	s.rememberUsers(users, hash)
}

func (s *Service) rememberUsers(users []model.UserSpec, hash string) {
	if hash == "" {
		hash = computeUserHash(users)
	}
	if users == nil {
		users = []model.UserSpec{}
	}
	s.metricsMu.Lock()
	s.lastUsers = cloneUsersNonNil(users)
	s.metricsMu.Unlock()
	s.lastUserHash = hash
}

// startKernel starts (or restarts) the kernel with the given config/users and
// records the successfully applied state. Returns false on error.
func (s *Service) startKernel(nc *model.NodeSpec, users []model.UserSpec) bool {
	return s.applyKernel(nc, users, false)
}

func (s *Service) stopKernel() {
	s.stopKernelInstance()
	if s.portHopping != nil {
		if err := s.portHopping.Close(); err != nil {
			nlog.Core().Error("failed to clean HY2 redirects", "error", err)
		}
	}
}

// FinalReportTimeout bounds waiting for an in-flight report and its final successor.
const FinalReportTimeout = time.Minute

func (s *Service) stopKernelInstance() {
	s.kernel.Stop()
	// Both backends retain cumulative counters across stops. Process only the
	// delta since the preceding sample, including on repeated stops.
	traffic, _, _, err := s.kernel.GetUserTraffic(context.Background())
	if err != nil {
		nlog.Core().Warn("failed to collect final traffic", "error", err)
	} else if len(traffic) != 0 {
		s.tracker.Process(traffic, nil, 0)
	}
	s.tracker.ClearActivity()
}

func (s *Service) applyKernel(nc *model.NodeSpec, users []model.UserSpec, reload bool) bool {
	return s.applyKernelWithTLS(nc, users, reload, s.cert.TLSCert()).applied
}

func isHysteria2(nc *model.NodeSpec) bool {
	return nc.Protocol == "hysteria2" || nc.Protocol == "hysteria" && nc.Version == 2
}

func (s *Service) trackKernelListener(nc *model.NodeSpec, activate func() error) error {
	if s.portHopping == nil {
		return activate()
	}
	if isHysteria2(nc) {
		return s.portHopping.TrackListener(nc.ListenIP, nc.ServerPort, activate)
	}
	return s.portHopping.TrackListener("", 0, activate)
}

func (s *Service) applyKernelWithTLS(nc *model.NodeSpec, users []model.UserSpec, reload bool, tlsCert kernel.TLSCert) (result kernelApplyResult) {
	portRange := ""
	if nc.HY2Settings != nil {
		portRange = nc.HY2Settings.PortRange
	}
	spec, err := porthopping.NewSpec(nc.ListenIP, portRange, nc.ServerPort)
	if err != nil {
		nlog.Core().Error("invalid HY2 redirects", "error", err)
		return result
	}
	wasStopped := s.clearActivityIfStopped()
	activate := func() error {
		if reload {
			if err := s.kernel.Reload(nc, users, tlsCert); err == nil {
				return nil
			} else {
				nlog.Core().Warn("reload failed, restarting", "error", err)
			}
		}
		return s.kernel.Start(nc, users, tlsCert)
	}
	rollback := func() error {
		s.stopKernelInstance()
		if old := s.appliedState.Config; old != nil && len(s.appliedState.Users) > 0 {
			if err := s.trackKernelListener(old, func() error {
				return s.kernel.Start(old, s.appliedState.Users, s.appliedState.TLSCert)
			}); err != nil {
				return err
			}
			result.rolledBackUsers = cloneUsersNonNil(s.appliedState.Users)
		}
		return nil
	}
	if s.portHopping != nil && spec == nil && isHysteria2(nc) {
		err = s.portHopping.ApplyScalar(nc.ListenIP, nc.ServerPort, activate, rollback)
	} else if s.portHopping != nil {
		if spec == nil {
			start := activate
			activate = func() error { return s.trackKernelListener(nc, start) }
		}
		err = s.portHopping.Apply(spec, activate, rollback)
	} else if spec != nil {
		err = fmt.Errorf("HY2 redirects manager is not initialized")
	} else {
		err = activate()
	}
	// Establish a timed counter baseline before the first tracking tick after a start.
	if (wasStopped && err == nil) || result.rolledBackUsers != nil {
		s.trackAndEnforce(context.Background())
	}
	if err != nil {
		nlog.Core().Error("failed to start kernel", "error", err)
		return result
	}

	s.publishUserPolicy(users)
	s.appliedState.Config = nc
	s.appliedState.Users = cloneUsersNonNil(users)
	s.appliedState.TLSCert = cloneTLSCert(tlsCert)

	// Initialize node logger on first successful start
	if s.nodeLog == nil {
		s.nodeLog = nlog.ForNode(nc.Protocol, nc.ServerPort)
	}
	s.speedTracker.SetLogCallback(func(msg string) {
		fullMsg := fmt.Sprintf("speedtracker: %s active_limiters=%d", msg, s.speedTracker.LimitedUserCount())
		s.nodeLog.Info(fullMsg)
	})
	s.nodeLog.Info(fmt.Sprintf("started, %d users", len(users)))
	result.applied = true
	return result
}

// ensureRunning starts the kernel if it is not running and there are users +
// config available. Returns true if the kernel is running afterwards.
func (s *Service) ensureRunning() bool {
	if s.kernel.IsRunning() {
		return true
	}
	if users := s.safeUsersForRestart(); len(users) > 0 && s.lastConfig != nil {
		return s.startKernel(s.lastConfig, users)
	}
	return false
}

// ─── User update entry points ───────────────────────────────────────────────

// applyUserUpdate replaces the full user set and hot-swaps the kernel.
// Called from WS sync.users and REST polling.
func (s *Service) applyUserUpdate(ctx context.Context, users []model.UserSpec, newHash string) bool {
	if users == nil {
		users = []model.UserSpec{}
	}

	if len(users) == 0 {
		s.restoreUserState(users, newHash)
		if s.kernel.IsRunning() {
			s.stopKernel()
			if s.nodeLog != nil {
				s.nodeLog.Info("users cleared (0 users), kernel stopped")
			}
		}
		s.appliedState.Users = nil
		return true
	}

	s.appliedState.Users = s.stageUserState(users, newHash)
	if s.lastConfig == nil {
		return true // staged for the first config
	}
	if !s.kernel.IsRunning() || s.usersNeedsReapply {
		return s.rebuildUsers(users)
	}
	added, removed, err := s.kernel.UpdateUsers(users)
	if err != nil {
		nlog.Core().Warn("user update failed, rebuilding from the requested list", "error", err)
		return s.rebuildUsers(users)
	}
	s.publishUserPolicy(users)
	s.appliedState.Users = cloneUsersNonNil(users)
	if s.nodeLog != nil && (added > 0 || removed > 0) {
		s.nodeLog.Info(fmt.Sprintf("users updated: +%d -%d", added, removed))
	}
	return true
}

// Do not let a kernel implementation restore its own stale per-user cache
// after a partial hot update. Only applyKernel's pruned snapshot can roll back.
func (s *Service) rebuildUsers(users []model.UserSpec) bool {
	if s.kernel.IsRunning() {
		s.stopKernelInstance()
	}
	result := s.applyKernelWithTLS(s.lastConfig, users, false, s.cert.TLSCert())
	if result.applied {
		s.rememberUsers(users, "")
	} else {
		s.recordUserApplyFailure(users, result.rolledBackUsers)
	}
	return result.applied
}

func (s *Service) recordUserApplyFailure(target, rolledBack []model.UserSpec) {
	s.usersNeedsReapply = true
	if rolledBack != nil {
		s.restoreUserState(rolledBack, computeUserHash(rolledBack))
	} else {
		if s.kernel.IsRunning() {
			s.stopKernelInstance()
		}
		s.stageUserState(target, "")
	}
	if !s.hasPendingForceResync() {
		s.wsResyncWanted.Add(1)
	}
}

// A WS delta cannot prove that a previously failed REST grant is still valid.
func (s *Service) restrictPendingUsers(target []model.UserSpec) {
	if s.configPending {
		target = model.IntersectUsers(s.appliedState.Users, target)
	}
	safe := s.stageUserState(target, "")
	if s.kernel.IsRunning() && computeUserHash(safe) != computeUserHash(s.appliedState.Users) {
		if len(safe) == 0 {
			s.stopKernel()
		} else if _, _, err := s.kernel.UpdateUsers(safe); err != nil {
			s.stopKernel()
		}
	}
	s.appliedState.Users = safe
}

func (s *Service) holdConfigGrants() {
	s.configPending = true
	if !s.hasPendingForceResync() {
		s.wsResyncWanted.Add(1)
	}
}

// applyUserDelta applies an incremental user change (add or remove) directly
// via the kernel's atomic user API. Kernel updates run before updateUserState.
func (s *Service) applyUserDelta(ctx context.Context, action string, deltaUsers []model.UserSpec) {
	if s.usersNeedsReapply || s.configPending {
		switch action {
		case "remove":
			s.restrictPendingUsers(subtractUsers(s.lastUsers, deltaUsers))
		case "add":
			s.restrictPendingUsers(mergeUsers(s.lastUsers, deltaUsers))
		}
		return
	}
	switch action {
	case "add":
		// Defensive check for empty or nil deltaUsers
		if deltaUsers == nil || len(deltaUsers) == 0 {
			return
		}
		merged := mergeUsers(s.lastUsers, deltaUsers)

		if !s.kernel.IsRunning() {
			s.appliedState.Users = s.stageUserState(merged, "")
			if s.lastConfig != nil && !s.rebuildUsers(merged) {
				s.pullViaAPIWithOptions(ctx, true)
			}
			return
		}

		hasUUIDChange := false
		for _, delta := range deltaUsers {
			for _, old := range s.lastUsers {
				if old.ID == delta.ID && old.UUID != delta.UUID {
					hasUUIDChange = true
					break
				}
			}
			if hasUUIDChange {
				break
			}
		}

		s.appliedState.Users = s.stageUserState(merged, "")
		var added int
		var err error
		if hasUUIDChange {
			added, _, err = s.kernel.UpdateUsers(merged)
		} else {
			added, err = s.kernel.AddUsers(deltaUsers)
			if err != nil {
				nlog.Core().Warn(fmt.Sprintf("AddUsers failed: %v, falling back to UpdateUsers", err))
				_, _, err = s.kernel.UpdateUsers(merged)
			}
		}
		if err != nil {
			nlog.Core().Warn(fmt.Sprintf("user delta add failed: %v, restarting kernel", err))
			if !s.rebuildUsers(merged) {
				s.pullViaAPIWithOptions(ctx, true)
				return
			}
		} else {
			s.publishUserPolicy(merged)
			s.appliedState.Users = cloneUsersNonNil(merged)
		}
		if s.nodeLog != nil && added > 0 {
			s.nodeLog.Info(fmt.Sprintf("users added: +%d", added))
		}

	case "remove":
		// Defensive check for empty or nil deltaUsers
		if deltaUsers == nil || len(deltaUsers) == 0 {
			return
		}
		filtered := subtractUsers(s.lastUsers, deltaUsers)

		if !s.kernel.IsRunning() {
			s.stageUserState(filtered, "")
			if len(s.appliedState.Users) > 0 {
				pruned := subtractUsers(s.appliedState.Users, deltaUsers)
				if len(pruned) == 0 {
					s.appliedState.Users = nil
				} else {
					s.appliedState.Users = cloneUsersNonNil(pruned)
				}
			}
			return
		}

		if len(filtered) == 0 {
			s.updateUserState(filtered)
			s.stopKernel()
			s.appliedState.Users = nil
			if s.nodeLog != nil {
				s.nodeLog.Info("last users removed (0 users), kernel stopped")
			}
			return
		}

		s.appliedState.Users = s.stageUserState(filtered, "")
		removed, err := s.kernel.RemoveUsers(deltaUsers)
		if err != nil {
			nlog.Core().Warn(fmt.Sprintf("RemoveUsers failed: %v, falling back to UpdateUsers", err))
			_, _, err = s.kernel.UpdateUsers(filtered)
		}
		if err != nil {
			nlog.Core().Warn(fmt.Sprintf("user delta remove failed: %v, restarting kernel", err))
			if !s.rebuildUsers(filtered) {
				s.pullViaAPIWithOptions(ctx, true)
				return
			}
		} else {
			s.publishUserPolicy(filtered)
			s.appliedState.Users = cloneUsersNonNil(filtered)
		}
		if s.nodeLog != nil && removed > 0 {
			s.nodeLog.Info(fmt.Sprintf("users removed: -%d", removed))
		}

	default:
		nlog.Core().Warn(fmt.Sprintf("unknown user delta action: %s", action))
	}
}

// mergeUsers overlays deltaUsers onto base (keyed by ID). New users are
// appended, existing users have their properties overwritten.
func mergeUsers(base, delta []model.UserSpec) []model.UserSpec {
	// Handle nil slices
	if base == nil {
		base = []model.UserSpec{}
	}
	if delta == nil {
		return base
	}

	m := make(map[int]model.UserSpec, len(base))
	for _, u := range base {
		m[u.ID] = u
	}
	for _, u := range delta {
		m[u.ID] = u
	}
	out := make([]model.UserSpec, 0, len(m))
	for _, u := range m {
		out = append(out, u)
	}
	return out
}

// subtractUsers returns base with all users in delta removed.
func subtractUsers(base, delta []model.UserSpec) []model.UserSpec {
	if base == nil {
		return nil
	}
	if delta == nil || len(delta) == 0 {
		return base
	}
	removeSet := make(map[int]struct{}, len(delta))
	for _, u := range delta {
		removeSet[u.ID] = struct{}{}
	}
	out := make([]model.UserSpec, 0, len(base))
	for _, u := range base {
		if _, ok := removeSet[u.ID]; !ok {
			out = append(out, u)
		}
	}
	return out
}

// During recovery the saved target may have changed again at the panel. A
// config-only update or renewal can restart only previously confirmed users
// that survived all the intervening revocations.
func (s *Service) safeUsersForRestart() []model.UserSpec {
	if s.usersNeedsReapply || s.configPending {
		return s.appliedState.Users
	}
	return s.lastUsers
}

func (s *Service) applyConfigWithUsers(users []model.UserSpec) kernelApplyResult {
	if len(users) == 0 {
		s.stopKernel()
		s.appliedState.Users = nil
		return kernelApplyResult{applied: true}
	}
	if s.lastConfig == nil {
		return kernelApplyResult{}
	}
	return s.applyKernelWithTLS(s.lastConfig, users, s.kernel.IsRunning(), s.cert.TLSCert())
}

// Covers a kernel that stopped outside an explicit service stop, including a failed reload.
func (s *Service) clearActivityIfStopped() bool {
	if !s.kernel.IsRunning() {
		s.tracker.ClearActivity()
		return true
	}
	return false
}

func (s *Service) trackAndEnforce(ctx context.Context) {
	if s.clearActivityIfStopped() {
		return
	}

	traffic, aliveIPs, connCount, err := s.kernel.GetUserTraffic(ctx)
	if err != nil {
		nlog.Core().Debug("get user traffic failed", "error", err)
		return
	}

	s.tracker.Process(traffic, aliveIPs, connCount)

	// Only log stats if there's actual traffic or connections
	if connCount > 0 || len(traffic) > 0 {
		if s.nodeLog != nil {
			s.nodeLog.Debug(fmt.Sprintf("tracker: %d conns, %d users online", connCount, len(traffic)))
		} else {
			nlog.TrackerStats(connCount, len(traffic))
		}
	}
}

// pushReportAsync sends the report in a background goroutine so the select
// loop is never blocked by slow HTTP. Only one push runs at a time.
func (s *Service) pushReportAsync() {
	if !s.sink.SupportsReporting() {
		return
	}
	if s.reportDone != nil {
		select {
		case <-s.reportDone:
		default:
			nlog.Core().Debug("push already in progress, skipping")
			return
		}
	}
	if s.pushBackoff.shouldSkip() {
		s.startTrafficReport() // telemetry backoff does not delay an outstanding receipt
		nlog.Core().Debug("skipping report due to backoff")
		return
	}
	s.startReport()
}

func (s *Service) trafficReporter() (*panel.Client, trafficSpool, bool) {
	provider, ok := s.sink.(interface{ BatchClient() *panel.Client })
	if !ok || provider.BatchClient() == nil {
		return nil, trafficSpool{}, false
	}
	client := provider.BatchClient()
	enabled, known := client.TrafficBatchCapability()
	spool := newTrafficSpool(s.cfg)
	spool.writeMu = &s.batchDisk
	if !enabled && !spool.exists() {
		return nil, spool, false
	}
	return client, spool, enabled || !known
}

func (s *Service) startTrafficReport() bool {
	client, spool, freeze := s.trafficReporter()
	if client == nil {
		return false
	}
	if s.batchDone != nil {
		select {
		case <-s.batchDone:
		default:
			return freeze
		}
	}
	done := make(chan struct{})
	s.batchDone = done
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := spool.report(ctx, client, s.tracker, freeze); err != nil {
			nlog.Core().Warn("traffic batch report pending", "error", err)
		}
		if stats, err := spool.status(); err == nil {
			s.batchStats.Store(stats)
		}
	}()
	return freeze
}

func (s *Service) startReport() {
	running := !s.clearActivityIfStopped()
	var traffic map[int][2]int64
	if !s.startTrafficReport() {
		traffic = s.tracker.FlushTraffic()
	}
	aliveIPs := s.tracker.FlushAliveIPs()
	online := s.tracker.CurrentOnline()
	status := monitor.Collect()
	metrics := s.buildMetrics(status, running)

	done := make(chan struct{})
	s.reportDone = done
	go func() {
		defer close(done)
		if err := s.sink.Report(controlplane.ReportPayload{Traffic: traffic, Alive: aliveIPs, Online: online, CPU: status.CPU, Mem: [2]uint64{status.MemTotal, status.MemUsed}, Swap: [2]uint64{status.SwapTotal, status.SwapUsed}, Disk: [2]uint64{status.DiskTotal, status.DiskUsed}, Metrics: metrics}); err != nil {
			nlog.Core().Warn("failed to push report", "error", err)
			if len(traffic) > 0 {
				s.tracker.RestoreTraffic(traffic)
			}
			s.pushBackoff.onFailure()
			return
		}
		s.pushBackoff.onSuccess()
		nlog.ReportPushed(len(traffic), len(online))
	}()
}

// Complete the in-flight report before the final snapshot. An older request
// must not republish devices after shutdown or restore traffic after the final flush.
func (s *Service) pushReportSync(timeout time.Duration) {
	if !s.sink.SupportsReporting() {
		return
	}
	// A slow prior HTTP request may outlive the process shutdown grace. Save sampled bytes first.
	if _, spool, freeze := s.trafficReporter(); freeze && s.tracker.HasTraffic() {
		if err := spool.checkpoint(s.tracker); err != nil {
			nlog.Core().Warn("traffic shutdown checkpoint incomplete", "error", err)
		}
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	if s.reportDone != nil {
		select {
		case <-s.reportDone:
		case <-deadline.C:
			nlog.Core().Warn("timed out waiting for prior report at shutdown")
			return
		}
	}
	if s.batchDone != nil {
		select {
		case <-s.batchDone:
		case <-deadline.C:
			nlog.Core().Warn("timed out waiting for prior traffic batch at shutdown")
			return
		}
	}
	s.startReport()
	for _, done := range []<-chan struct{}{s.reportDone, s.batchDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-deadline.C:
			nlog.Core().Warn("final report timed out at shutdown")
			return
		}
	}
}

// buildMetrics aggregates node-level metrics to be reported to the panel.
// This includes active connections, per-core CPU, GC stats, API call stats,
// WebSocket status, and limiter hit counts.
func (s *Service) buildMetrics(status monitor.Status, running bool) map[string]interface{} {
	s.metricsMu.RLock()
	lastUsers := s.lastUsers
	wsClient := s.wsClient
	s.metricsMu.RUnlock()

	m := make(map[string]interface{})
	m["kernel_status"] = running
	if stats := s.batchStats.Load(); stats != nil {
		var age int64
		if !stats.Oldest.IsZero() {
			age = max(0, int64(time.Since(stats.Oldest).Seconds()))
		}
		m["traffic_batches"] = map[string]interface{}{"pending": stats.Pending, "oldest_age_seconds": age}
	}
	var activeConnections, activeUsers int
	var inboundSpeed, outboundSpeed int64
	if running {
		activeConnections = s.tracker.ActiveConnections()
		activeUsers = len(s.tracker.CurrentOnline())
		inboundSpeed = s.tracker.InboundSpeed()
		outboundSpeed = s.tracker.OutboundSpeed()
	}

	m["uptime"] = status.Uptime
	m["goroutines"] = status.Goroutines

	// Active connections (last measured during tracker.Process()).
	m["active_connections"] = activeConnections
	m["total_connections"] = s.tracker.TotalConnections()
	m["active_users"] = activeUsers
	m["total_users"] = len(lastUsers)

	// Speed
	m["inbound_speed"] = inboundSpeed
	m["outbound_speed"] = outboundSpeed

	// Per-core CPU usage (if available).
	if len(status.CPUPerCore) > 0 {
		m["cpu_per_core"] = status.CPUPerCore
	}

	m["load"] = map[string]interface{}{
		"load1":  status.Load1,
		"load5":  status.Load5,
		"load15": status.Load15,
	}

	// Speed Limiter metrics
	m["speed_limiter"] = map[string]interface{}{
		"has_limits":    s.speedTracker.HasLimits(),
		"limited_users": s.speedTracker.LimitedUserCount(),
	}

	// GC metrics.
	m["gc"] = map[string]interface{}{
		"num_gc":        status.NumGC,
		"last_pause_ms": status.LastPauseMS,
	}

	// API metrics.
	api := s.source.Metrics()
	m["api"] = map[string]interface{}{
		"success": api.Success,
		"failure": api.Failure,
	}

	// WebSocket status.
	wsEnabled := wsClient != nil
	if mux, ok := wsClient.(interface{ IsEnabled() bool }); ok {
		wsEnabled = mux.IsEnabled()
	}
	wsConnected := wsEnabled && wsClient.IsConnected()
	m["ws"] = map[string]interface{}{
		"enabled":   wsEnabled,
		"connected": wsConnected,
	}

	// Limiter metrics.
	lm := s.limiter.SnapshotMetrics()
	m["limits"] = map[string]interface{}{
		"device_limit_events": lm.DeviceLimitEvents,
		"speed_limited_users": s.speedTracker.LimitedUserCount(),
	}

	return m
}

// computeConfigHash returns a deterministic hash of the node config.
// It uses JSON marshaling to ensure all fields are captured, ensuring that
// any configuration change correctly triggers a kernel reload.
func computeConfigHash(cfg *model.NodeSpec) string {
	if cfg == nil {
		return ""
	}
	h := sha256.New()
	// We marshal the entire config to be safe. Node config updates are low-frequency,
	// so the robustness of capturing all fields outweighs the micro-performance of manual hashing.
	data, _ := json.Marshal(cfg)
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// computeUserHash returns a deterministic hash of the user list for change detection.
// Uses direct byte encoding instead of binary.Write to avoid reflection overhead.
func computeUserHash(users []model.UserSpec) string {
	sorted := make([]model.UserSpec, len(users))
	copy(sorted, users)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	h := sha256.New()
	var buf [8]byte
	for _, u := range sorted {
		binary.LittleEndian.PutUint64(buf[:], uint64(u.ID))
		h.Write(buf[:])
		io.WriteString(h, u.UUID)
		binary.LittleEndian.PutUint64(buf[:], uint64(u.SpeedLimit))
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(u.DeviceLimit))
		h.Write(buf[:])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ─── Device management ──────────────────────────────────────────────────

// sendDeviceBatch reports local device snapshot to panel via WS.
func (s *Service) sendDeviceBatch() {
	if s.wsClient == nil || !s.wsClient.IsConnected() {
		return
	}

	s.clearActivityIfStopped()
	devices := s.tracker.FlushAliveIPs()
	s.sink.ReportDevices(s.wsClient, devices)
	nlog.Core().Debug("device snapshot sent", "users", len(devices))
}

// reportDevices periodically reports device snapshot to panel.
func (s *Service) reportDevices() {
	s.sendDeviceBatch()
}

// ─── Runtime validation ─────────────────────────────────────────────────

func validateNodeRuntime(cfg *config.Config, kcfgSupported []string, spec *model.NodeSpec, tls kernel.TLSCert) error {
	if err := validateCandidateFields(cfg, kcfgSupported, spec); err != nil {
		return err
	}
	return validateTLSRequirements(spec, tls, cfgKernelType(cfg))
}

// Certificate requirements are checked after preparing the candidate certificate.
func validateCandidateFields(cfg *config.Config, kcfgSupported []string, spec *model.NodeSpec) error {
	if spec == nil {
		return fmt.Errorf("node spec is nil")
	}
	if !containsString(kcfgSupported, spec.Protocol) {
		return fmt.Errorf("protocol %q is not supported by kernel %q", spec.Protocol, cfg.Kernel.Type)
	}
	if spec.TLS == 2 {
		if err := validateRealityRequirements(spec, cfgKernelType(cfg)); err != nil {
			return err
		}
	}
	if err := validateRuntimeCertConfig(spec); err != nil {
		return err
	}
	return nil
}

func validateTLSRequirements(spec *model.NodeSpec, tls kernel.TLSCert, kernelType string) error {
	needsCert := false
	switch spec.Protocol {
	case "hysteria", "hysteria2", "tuic", "anytls":
		needsCert = true
	case "trojan":
		if spec.TLS != 2 {
			needsCert = true
		}
	}
	if needsCert && !hasUsableTLSConfig(spec, tls) {
		return fmt.Errorf("protocol %q requires TLS certificate files", spec.Protocol)
	}
	if spec.TLS == 2 {
		if err := validateRealityRequirements(spec, kernelType); err != nil {
			return err
		}
	}
	return nil
}

func hasUsableTLSConfig(spec *model.NodeSpec, tls kernel.TLSCert) bool {
	if tls.HasCert() {
		return true
	}
	if spec == nil || spec.CertConfig == nil {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(spec.CertConfig.CertMode))
	switch mode {
	case "self":
		return true
	case "content":
		return strings.TrimSpace(spec.CertConfig.CertContent) != "" && strings.TrimSpace(spec.CertConfig.KeyContent) != ""
	case "file":
		return strings.TrimSpace(spec.CertConfig.CertFile) != "" && strings.TrimSpace(spec.CertConfig.KeyFile) != ""
	case "http":
		return strings.TrimSpace(spec.CertConfig.Domain) != ""
	case "dns":
		return strings.TrimSpace(spec.CertConfig.Domain) != "" && strings.TrimSpace(spec.CertConfig.DNSProvider) != ""
	default:
		return false
	}
}

func validateRuntimeCertConfig(spec *model.NodeSpec) error {
	if spec == nil || spec.CertConfig == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(spec.CertConfig.CertMode))
	if mode != "dns" {
		return nil
	}
	provider := strings.TrimSpace(spec.CertConfig.DNSProvider)
	if provider == "" {
		return fmt.Errorf("dns cert mode requires cert_config.dns_provider")
	}
	if _, ok := dnsproviders.Get(provider); !ok {
		return fmt.Errorf("unsupported cert_config.dns_provider %q (supported: %s)", provider, strings.Join(dnsproviders.CanonicalNames(), ", "))
	}
	return nil
}

func validateRealityRequirements(spec *model.NodeSpec, _ string) error {
	if spec.TLSSettings == nil {
		return fmt.Errorf("reality tls requires tls_settings")
	}
	privateKey := strings.TrimSpace(stringValue(spec.TLSSettings["private_key"]))
	serverName := strings.TrimSpace(stringValue(spec.TLSSettings["server_name"]))
	dest := strings.TrimSpace(stringValue(spec.TLSSettings["dest"]))
	if privateKey == "" {
		return fmt.Errorf("reality tls requires tls_settings.private_key")
	}
	if serverName == "" && dest == "" {
		return fmt.Errorf("reality tls requires tls_settings.server_name or tls_settings.dest")
	}
	return nil
}

func cfgKernelType(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(cfg.Kernel.Type))
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func stringValue(v any) string {
	switch value := v.(type) {
	case string:
		return value
	default:
		return ""
	}
}
