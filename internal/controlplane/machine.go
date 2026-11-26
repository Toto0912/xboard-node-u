package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/panel"
)

// MachinePanelControlPlane implements ControlPlane for a single node running
// under the machine orchestrator. REST calls go through a per-node panel.Client
// with machine-level auth; WS events are delivered via an externally-managed
// PushClient supplied by the machine WS mux.
type MachinePanelControlPlane struct {
	client *panel.Client
	kcfg   config.KernelConfig
	push   PushClient // virtual push client from WS mux (may be nil)

	registerFn func(statuses *StatusMailbox) *NodeMailbox
	handoff    MachineKernelHandoff
}

// NewMachinePanelControlPlane creates a per-node control plane for machine mode.
// push may be nil if WS is unavailable.
// registerFn is called during Initial() to wire machine mailbox access and status
// updates into the service loop. It may be nil when WS is not used.
func NewMachinePanelControlPlane(
	client *panel.Client,
	kcfg config.KernelConfig,
	push PushClient,
	registerFn func(statuses *StatusMailbox) *NodeMailbox,
) *MachinePanelControlPlane {
	return &MachinePanelControlPlane{
		client:     client,
		kcfg:       kcfg,
		push:       push,
		registerFn: registerFn,
	}
}

// MachineKernelHandoff carries one child generation's transition hooks.
type MachineKernelHandoff struct {
	Mismatch  func()
	Rollback  *model.NodeSpec
	Confirmed *[sha256.Size]byte
	Applied   func()
}

// MachineConfigDigest identifies the decoded panel descriptor independent of map order and ETags.
func MachineConfigDigest(cfg *panel.NodeConfig) [sha256.Size]byte {
	data, _ := json.Marshal(cfg) // The descriptor came from a JSON payload.
	return sha256.Sum256(data)
}

// SetKernelHandoff configures the machine lifecycle callbacks before the service starts.
func (p *MachinePanelControlPlane) SetKernelHandoff(h MachineKernelHandoff) { p.handoff = h }

func (p *MachinePanelControlPlane) SupportsPolling() bool       { return true }
func (p *MachinePanelControlPlane) SupportsDiscovery() bool     { return false }
func (p *MachinePanelControlPlane) SupportsReporting() bool     { return true }
func (p *MachinePanelControlPlane) SupportsDeviceReports() bool { return p.push != nil }
func (p *MachinePanelControlPlane) ResetConfigETag()            { p.client.ResetConfigETag() }
func (p *MachinePanelControlPlane) ResetUserETag()              { p.client.ResetUserETag() }

func (p *MachinePanelControlPlane) Initial(
	ctx context.Context,
	metricsFn func() map[string]interface{},
	events chan<- Event,
	statuses *StatusMailbox,
) (Bootstrap, error) {
	// In machine mode the orchestrator handles the shared WS handshake, but each
	// node service still needs one initial REST snapshot so newly discovered nodes
	// can start immediately instead of waiting for a subsequent WS full-sync.
	hs, err := p.client.HandshakeContext(ctx)
	if err != nil {
		nlog.Core().Debug("machine node handshake skipped, using REST", "error", err)
	}

	bootstrap := Bootstrap{}
	if hs != nil {
		bootstrap.PushInterval = hs.Settings.PushInterval
		bootstrap.PullInterval = hs.Settings.PullInterval
	}

	configSnapshot, cfgETag, err := p.client.GetConfigContext(ctx)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("machine initial config: %w", err)
	}
	if configSnapshot == nil {
		return Bootstrap{}, fmt.Errorf("machine initial config is nil")
	}
	if expected := p.handoff.Confirmed; expected != nil && *expected != MachineConfigDigest(configSnapshot) {
		return Bootstrap{}, fmt.Errorf("machine kernel handoff config changed since preflight")
	}
	users, userETag, err := p.client.GetUsersContext(ctx)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("machine initial users: %w", err)
	}
	if users == nil && (p.handoff.Confirmed != nil || p.handoff.Rollback != nil) {
		return Bootstrap{}, fmt.Errorf("machine handoff requires a full user list")
	}
	if p.handoff.Rollback != nil && model.ResolveKernelForTransport(configSnapshot.Network, p.kcfg.Type) != p.kcfg.Type {
		// Use the old descriptor with the newly authorized users. Leave the panel
		// config unacknowledged so the next poll can retry the handoff.
		bootstrap.Config = p.handoff.Rollback
		cfgETag = ""
	} else {
		bootstrap.Config, err = model.NodeSpecFromPanelValidated(configSnapshot, p.kcfg)
	}
	if err != nil {
		return Bootstrap{}, fmt.Errorf("machine initial config normalize: %w", err)
	}
	if bootstrap.PushInterval == 0 && configSnapshot.BaseConfig.PushInterval > 0 {
		bootstrap.PushInterval = configSnapshot.BaseConfig.PushInterval
	}
	if bootstrap.PullInterval == 0 && configSnapshot.BaseConfig.PullInterval > 0 {
		bootstrap.PullInterval = configSnapshot.BaseConfig.PullInterval
	}
	bootstrap.CommitConfig = func() {
		p.client.CommitConfigETag(cfgETag)
		p.handoff.Rollback = nil
		p.handoff.Confirmed = nil
		if p.handoff.Applied != nil {
			p.handoff.Applied()
		}
	}
	bootstrap.CommitUsers = func() { p.client.CommitUserETag(userETag) }
	bootstrap.Users = model.UserSpecsFromPanel(users)

	// Register mailbox access after the initial snapshot is prepared, so the
	// Service can start the kernel immediately and only then drain coalesced WS
	// state from the machine mailbox.
	if p.push != nil {
		if p.registerFn != nil {
			bootstrap.Mailbox = p.registerFn(statuses)
		}
		bootstrap.Push = p.push
	}

	return bootstrap, nil
}

func (p *MachinePanelControlPlane) Poll(ctx context.Context) (Snapshot, error) {
	configSnapshot, cfgETag, err := p.client.GetConfigContext(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("machine poll config: %w", err)
	}
	// Process user revocations even while the new transport is incompatible.
	if ctx.Err() == nil && configSnapshot != nil && p.handoff.Mismatch != nil &&
		model.ResolveKernelForTransport(configSnapshot.Network, p.kcfg.Type) != p.kcfg.Type {
		p.handoff.Mismatch()
		configSnapshot, cfgETag = nil, ""
	}
	users, userETag, err := p.client.GetUsersContext(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("machine poll users: %w", err)
	}
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	default:
	}
	nodeSpec, err := model.NodeSpecFromPanelValidated(configSnapshot, p.kcfg)
	if err != nil {
		return Snapshot{Users: model.UserSpecsFromPanel(users)}, fmt.Errorf("machine poll normalize: %w", err)
	}
	return Snapshot{
		Config:       nodeSpec,
		Users:        model.UserSpecsFromPanel(users),
		CommitConfig: func() { p.client.CommitConfigETag(cfgETag) },
		CommitUsers:  func() { p.client.CommitUserETag(userETag) },
	}, nil
}

func (p *MachinePanelControlPlane) Discover(
	ctx context.Context,
	metricsFn func() map[string]interface{},
	events chan<- Event,
	statuses *StatusMailbox,
) (PushClient, error) {
	return nil, nil
}

func (p *MachinePanelControlPlane) BatchClient() *panel.Client { return p.client }

func (p *MachinePanelControlPlane) Report(payload ReportPayload) error {
	return p.client.Report(
		payload.Traffic, payload.Alive, payload.Online,
		payload.CPU, payload.Mem, payload.Swap, payload.Disk,
		payload.Metrics,
	)
}

func (p *MachinePanelControlPlane) ReportDevices(push PushClient, devices map[int][]string) {
	if push != nil {
		push.SendDeviceReport(devices)
	}
}

func (p *MachinePanelControlPlane) Metrics() APIMetrics {
	api := p.client.SnapshotMetrics()
	return APIMetrics{Success: api.Success, Failure: api.Failure}
}
