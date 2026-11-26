// Package machine implements the machine-mode orchestrator that dynamically
// discovers nodes from the panel's machine API and manages their lifecycles.
package machine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/monitor"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/service"
)

// nodeHandle tracks a running node service.
type nodeHandle struct {
	cancel  context.CancelFunc
	done    chan struct{}
	mailbox *controlplane.NodeMailbox
	svc     *service.Service
	// Protected by Orchestrator.mu. An ordinary WS config invalidates a probe
	// already in flight. Repeated XHTTP hints share the pending probe.
	ready       bool
	checkKernel bool
	configEpoch uint64
	lastHint    [sha256.Size]byte
	nextCheck   time.Time
	// Only a failed replacement gets a single immediate sing-box recovery.
	rollback *model.NodeSpec
}

// Orchestrator manages all nodes bound to a panel machine. It:
//   - discovers nodes via GET /machine/nodes
//   - starts / stops Service instances as nodes are added / removed
//   - maintains a shared WS connection that demuxes events by node_id
//   - reports machine-level load via POST /machine/status
type Orchestrator struct {
	cfg    *config.Config
	client *panel.Client // machine-level client (no node_id)

	mu    sync.Mutex
	nodes map[int]*nodeHandle // node_id → handle

	// Per-node mailbox keyed by node_id. Shared WS events are aggregated here
	// and each node service drains the latest state when ready.
	eventsMu  sync.RWMutex
	mailboxes map[int]*controlplane.NodeMailbox
	statuses  map[int]chan<- controlplane.StatusChange
	kernels   map[int]config.KernelConfig

	// Shared WS client (nil when WS is disabled).
	ws            atomic.Pointer[panel.WSClient]
	nextWSAttempt time.Time
	wsCancel      context.CancelFunc

	// Discovery runs only in Run. WS callbacks invalidate any older snapshot
	// under mu before waking the loop, including while a node is starting.
	discoveryWake chan struct{}
	generation    uint64
	stopPending   bool
	wsConnected   bool
	initialized   bool

	pullInterval time.Duration
	pushInterval time.Duration
}

// New creates a machine orchestrator from the given config.
func New(cfg *config.Config) *Orchestrator {
	panelCfg := config.PanelConfig{
		URL:       cfg.Panel.URL,
		Token:     cfg.Machine.Token,
		MachineID: cfg.Machine.MachineID,
	}
	return &Orchestrator{
		cfg:           cfg,
		client:        panel.NewClient(panelCfg),
		nodes:         make(map[int]*nodeHandle),
		mailboxes:     make(map[int]*controlplane.NodeMailbox),
		statuses:      make(map[int]chan<- controlplane.StatusChange),
		kernels:       make(map[int]config.KernelConfig),
		discoveryWake: make(chan struct{}, 1),
	}
}

// Run is the main loop. It blocks until ctx is cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.applyIntervals(panel.MachineBaseConfig{})
	discoveryTicker := time.NewTicker(o.pullInterval)
	statusTicker := time.NewTicker(o.pushInterval)
	defer discoveryTicker.Stop()
	defer statusTicker.Stop()

	discover := func() {
		initialized := o.initialized
		o.rediscover(ctx)
		if !initialized && o.initialized {
			discoveryTicker.Reset(o.pullInterval)
			statusTicker.Reset(o.pushInterval)
		}
	}
	discover()
	for {
		select {
		case <-ctx.Done():
			if o.wsCancel != nil {
				o.wsCancel()
			}
			o.stopChildren()
			return nil

		case <-discoveryTicker.C:
			discover()
		case <-o.discoveryWake:
			discover()

		case <-statusTicker.C:
			o.reportMachineStatus()
		}
	}
}

// ─── Node lifecycle ──────────────────────────────────────────────────────

func (o *Orchestrator) startNode(ctx context.Context, mn panel.MachineNode, generation uint64) {
	o.launchNode(ctx, mn, generation, nil)
}

type kernelReplacement struct {
	previous  *model.NodeSpec
	confirmed [sha256.Size]byte
}

func (o *Orchestrator) launchNode(ctx context.Context, mn panel.MachineNode, generation uint64, replacement *kernelReplacement) {
	o.mu.Lock()
	if o.generation != generation || ctx.Err() != nil {
		o.mu.Unlock()
		return
	}
	var rollback *model.NodeSpec
	if previous, exists := o.nodes[mn.ID]; exists {
		select {
		case <-previous.done:
			previous.cancel()
			rollback = previous.rollback
			delete(o.nodes, mn.ID)
		default:
			o.mu.Unlock()
			o.checkKernelSwitch(ctx, mn, generation, previous)
			return
		}
	}

	nodeCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	mb := controlplane.NewNodeMailbox()
	h := &nodeHandle{cancel: cancel, done: done, mailbox: mb}
	if rollback != nil {
		h.nextCheck = time.Now().Add(o.pullInterval)
		h.checkKernel = true
		h.rollback = rollback
	}
	o.nodes[mn.ID] = h
	o.mu.Unlock()

	o.eventsMu.Lock()
	o.mailboxes[mn.ID] = mb
	o.eventsMu.Unlock()

	nodeCfg := o.cfg.ExpandMachineNode(mn.ID, mn.Type)

	perNodeClient := o.client.ForNode(mn.ID)

	// Pre-fetch node config to detect transport-based kernel requirements.
	// If the transport (e.g. xhttp) is incompatible with the configured kernel
	// (e.g. singbox), auto-switch to the required kernel for this node.
	var confirmed *[sha256.Size]byte
	if replacement != nil {
		nodeCfg.Kernel.Type = "xray"
		h.rollback = replacement.previous
		confirmed = &replacement.confirmed
	} else if rollback == nil {
		if cfgSnapshot, err := perNodeClient.GetConfig(); err == nil && cfgSnapshot != nil {
			nodeCfg.Kernel.Type = model.ResolveKernelForTransport(cfgSnapshot.Network, nodeCfg.Kernel.Type)
		}
	} else {
		nlog.Core().Warn("machine: recovering previous sing-box config", "node_id", mn.ID)
	}
	// Reset cached ETag so the subsequent GetConfig in Initial() gets a full response.
	o.eventsMu.Lock()
	o.kernels[mn.ID] = nodeCfg.Kernel
	o.eventsMu.Unlock()
	perNodeClient.ResetConfigETag()

	// Children keep a stable adapter and mailbox even during REST-only startup.
	push := &machineNodePush{nodeID: mn.ID, mux: &o.ws}

	// The registerFn is called by MachinePanelControlPlane.Initial() to expose
	// the node mailbox + status channel to the Service.
	nodeID := mn.ID
	registerFn := func(st chan<- controlplane.StatusChange) *controlplane.NodeMailbox {
		o.registerNode(nodeID, st)
		return mb
	}

	cp := controlplane.NewMachinePanelControlPlane(perNodeClient, nodeCfg.Kernel, push, registerFn)
	replacing := replacement != nil
	cp.SetKernelHandoff(controlplane.MachineKernelHandoff{
		Mismatch: func() { o.requestKernelCheck(nodeID, mb) },
		Rollback: rollback, Confirmed: confirmed,
		Applied: func() {
			o.mu.Lock()
			h.ready = true
			h.rollback = nil
			wake := h.checkKernel && !time.Now().Before(h.nextCheck)
			o.mu.Unlock()
			if wake {
				o.wakeDiscovery()
			}
		},
	})
	svc := service.NewWithControlPlane(nodeCfg, cp)
	h.svc = svc

	nlog.Core().Info(fmt.Sprintf("machine: starting node %d (%s/%s)",
		mn.ID, mn.Type, mn.Name))

	go func() {
		// Cleanup and close done before asking the main loop to recover.
		if replacing {
			defer func() {
				if nodeCtx.Err() == nil {
					o.wakeDiscovery()
				}
			}()
		}
		defer close(done)
		defer o.unregisterNode(mn.ID)
		if nodeCtx.Err() != nil {
			return
		}
		if err := svc.Run(nodeCtx); err != nil {
			nlog.Core().Error("machine node exited with error",
				"node_id", mn.ID, "error", err)
		}
	}()
}

// requestKernelCheck is safe from the WS mux and the child's async poller.
// Binding to the mailbox makes late hints from a stopped child harmless.
func (o *Orchestrator) requestKernelCheck(nodeID int, mb *controlplane.NodeMailbox) {
	o.noteKernelHint(nodeID, mb, nil)
}

func (o *Orchestrator) noteKernelHint(nodeID int, mb *controlplane.NodeMailbox, hint *panel.NodeConfig) {
	var hash [sha256.Size]byte
	if hint != nil {
		hash = controlplane.MachineConfigDigest(hint)
	}
	o.mu.Lock()
	h := o.nodes[nodeID]
	wake := false
	if h != nil && h.mailbox == mb {
		wake = h.ready && !h.checkKernel && !time.Now().Before(h.nextCheck)
		if hint != nil && h.lastHint != hash {
			h.configEpoch++
			h.lastHint = hash
		}
		h.checkKernel = true
	}
	o.mu.Unlock()
	if wake {
		o.wakeDiscovery()
	}
}

func (o *Orchestrator) checkKernelSwitch(ctx context.Context, mn panel.MachineNode, generation uint64, h *nodeHandle) {
	o.mu.Lock()
	if !h.ready || !h.checkKernel || time.Now().Before(h.nextCheck) || o.generation != generation {
		o.mu.Unlock()
		return
	}
	epoch := h.configEpoch
	h.nextCheck = time.Now().Add(o.pullInterval)
	o.mu.Unlock()

	// Use fresh ETags so a hint or a rejected candidate never confirms itself.
	client := o.client.ForNode(mn.ID)
	raw, _, err := client.GetConfigWithETag()
	if err != nil || raw == nil {
		nlog.Core().Warn("machine: kernel check could not confirm config", "node_id", mn.ID, "error", err)
		return
	}
	if model.ResolveKernelForTransport(raw.Network, "singbox") != "xray" {
		o.mu.Lock()
		if h.configEpoch == epoch {
			h.checkKernel = false
		}
		o.mu.Unlock()
		return
	}
	cfg := o.cfg.ExpandMachineNode(mn.ID, mn.Type)
	cfg.Kernel.Type = "xray"
	confirmed := controlplane.MachineConfigDigest(raw)
	spec, err := model.NodeSpecFromPanelValidated(raw, cfg.Kernel)
	if err == nil {
		var users []panel.User
		users, _, err = client.GetUsersWithETag()
		if err == nil && users == nil {
			err = fmt.Errorf("expected a full user list for kernel handoff")
		}
		if err == nil {
			err = h.svc.ValidateXrayHandoff(cfg, spec, model.UserSpecsFromPanel(users))
		}
	}
	if err != nil {
		nlog.Core().Warn("machine: keeping existing kernel after preflight", "node_id", mn.ID, "error", err)
		return
	}
	o.mu.Lock()
	valid := o.nodes[mn.ID] == h && h.checkKernel && h.configEpoch == epoch && o.generation == generation && ctx.Err() == nil
	if valid {
		h.cancel()
	}
	o.mu.Unlock()
	if !valid {
		return
	}
	nlog.Core().Info("machine: switching child to Xray", "node_id", mn.ID, "transport", raw.Network)
	o.stopNode(mn.ID)
	o.launchNode(ctx, mn, generation, &kernelReplacement{previous: h.svc.StoppedConfig(), confirmed: confirmed})
}

func (o *Orchestrator) stopNode(nodeID int) {
	o.mu.Lock()
	h, ok := o.nodes[nodeID]
	if !ok {
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()

	o.eventsMu.Lock()
	delete(o.mailboxes, nodeID)
	o.eventsMu.Unlock()

	nlog.Core().Info(fmt.Sprintf("machine: stopping node %d", nodeID))
	h.cancel()
	<-h.done
	// Keep the handle visible until all cleanup has completed. Discovery must
	// not start another generation while its old firewall owner is stopping.
	o.mu.Lock()
	if o.nodes[nodeID] == h {
		delete(o.nodes, nodeID)
	}
	o.mu.Unlock()
}

func (o *Orchestrator) stopChildren() {
	o.mu.Lock()
	handles := make(map[int]*nodeHandle, len(o.nodes))
	for id, h := range o.nodes {
		handles[id] = h
	}
	o.mu.Unlock()

	for id, h := range handles {
		nlog.Core().Info(fmt.Sprintf("machine: stopping node %d", id))
		h.cancel()
	}
	for _, h := range handles {
		<-h.done
	}
	o.mu.Lock()
	for id, h := range handles {
		if o.nodes[id] == h {
			delete(o.nodes, id)
		}
	}
	o.mu.Unlock()
}

// ─── Node discovery ──────────────────────────────────────────────────────

func (o *Orchestrator) rediscover(ctx context.Context) {
	o.mu.Lock()
	generation, stop := o.generation, o.stopPending
	o.stopPending = false
	o.mu.Unlock()
	if stop {
		o.stopChildren()
	}
	nodesResp, err := o.client.GetMachineNodes()
	if err != nil {
		if errors.Is(err, panel.ErrMachineUnauthorized) {
			o.stopChildren()
		}
		nlog.Core().Warn("machine node discovery failed", "error", err)
		return
	}

	o.mu.Lock()
	stale := o.generation != generation
	o.mu.Unlock()
	if stale || ctx.Err() != nil {
		return
	}
	if !o.initialized {
		o.applyIntervals(nodesResp.BaseConfig)
		o.initialized = true
	}
	if o.ws.Load() == nil && !time.Now().Before(o.nextWSAttempt) {
		o.nextWSAttempt = time.Now().Add(o.pullInterval)
		o.tryStartWS(ctx)
	}
	if len(nodesResp.Nodes) == 0 {
		o.stopChildren()
		return
	}
	wanted := make(map[int]panel.MachineNode, len(nodesResp.Nodes))
	for _, n := range nodesResp.Nodes {
		wanted[n.ID] = n
	}

	o.mu.Lock()
	var toRemove []int
	for id := range o.nodes {
		if _, ok := wanted[id]; !ok {
			toRemove = append(toRemove, id)
		}
	}
	o.mu.Unlock()

	for _, id := range toRemove {
		o.stopNode(id)
	}

	for _, n := range nodesResp.Nodes {
		o.startNode(ctx, n, generation) // no-op if already running
	}
}

// ─── Machine status reporting ────────────────────────────────────────────

func (o *Orchestrator) reportMachineStatus() {
	s := monitor.Collect()
	if err := o.client.ReportMachineStatus(
		s.CPU,
		[2]uint64{s.MemTotal, s.MemUsed},
		[2]uint64{s.SwapTotal, s.SwapUsed},
		[2]uint64{s.DiskTotal, s.DiskUsed},
		s.NetInSpeed, s.NetOutSpeed,
	); err != nil {
		nlog.Core().Warn("machine status report failed", "error", err)
	}
}

// ─── WS mux ─────────────────────────────────────────────────────────────

func (o *Orchestrator) tryStartWS(ctx context.Context) {
	hs, err := o.client.Handshake()
	if err != nil {
		nlog.Core().Warn("machine ws handshake failed, REST only", "error", err)
		return
	}
	if !hs.WebSocket.Enabled || hs.WebSocket.WSURL == "" {
		nlog.Core().Info("machine: ws disabled by panel, REST only")
		return
	}

	wsCfg := panel.WSClientConfig{
		StatusInterval:   time.Duration(o.cfg.WS.StatusInterval) * time.Second,
		HandshakeTimeout: time.Duration(o.cfg.WS.HandshakeTimeout) * time.Second,
		BackoffInitial:   time.Duration(o.cfg.WS.BackoffInitial) * time.Second,
		BackoffMax:       time.Duration(o.cfg.WS.BackoffMax) * time.Second,
		MachineID:        o.cfg.Machine.MachineID,
	}

	ws := panel.NewWSClient(
		hs.WebSocket.WSURL,
		o.cfg.Machine.Token,
		0, // no single node_id
		wsCfg,
		o.onWSEvent,
		o.onWSStatus,
		nil, // per-node status is sent via machineNodePush
	)

	wsCtx, wsCancel := context.WithCancel(ctx)
	o.wsCancel = wsCancel
	o.ws.Store(ws)
	go ws.Run(wsCtx)

	nlog.Core().Info("machine: ws mux started")
}

// onWSEvent routes a WS event to the correct node's channel.
// sync.nodes is a machine-level event that triggers immediate rediscovery.
func (o *Orchestrator) onWSEvent(event panel.WSEvent) {
	if event.Type == panel.WSEventSyncNodes || event.Type == panel.WSEventMachineRevoked {
		o.requestDiscovery(event.Type == panel.WSEventMachineRevoked)
		return
	}

	nodeID := event.NodeID
	if nodeID == 0 {
		nlog.Core().Debug("machine ws event missing node_id, dropping", "type", event.Type)
		return
	}

	o.eventsMu.RLock()
	kcfg, initialized := o.kernels[nodeID]
	mailbox, ok := o.mailboxes[nodeID]
	o.eventsMu.RUnlock()
	if !initialized || !ok {
		// The initial REST snapshot covers events before kernel selection finishes.
		return
	}
	if event.Type == panel.WSEventSyncConfig && event.Config != nil && kcfg.Type == "singbox" {
		if model.ResolveKernelForTransport(event.Config.Network, kcfg.Type) != kcfg.Type {
			o.noteKernelHint(nodeID, mailbox, event.Config)
			return
		}
		o.mu.Lock()
		if h := o.nodes[nodeID]; h != nil && h.mailbox == mailbox {
			h.configEpoch++
			h.checkKernel = false
		}
		o.mu.Unlock()
	}
	translated, err := controlplane.TranslateWSEvent(event, kcfg)
	if err != nil {
		nlog.Core().Warn("machine ws event translation failed",
			"type", event.Type, "node_id", nodeID, "error", err)
		return
	}

	mailbox.Apply(translated)
}

func (o *Orchestrator) requestDiscovery(revoked bool) {
	o.mu.Lock()
	o.generation++
	if revoked {
		o.stopPending = true
		for _, h := range o.nodes {
			h.cancel()
		}
	}
	o.mu.Unlock()
	o.wakeDiscovery()
}

func (o *Orchestrator) wakeDiscovery() {
	select {
	case o.discoveryWake <- struct{}{}:
	default:
	}
}

// onWSStatus broadcasts WS connectivity changes to all registered nodes.
func (o *Orchestrator) onWSStatus(status panel.WSStatusChange) {
	o.eventsMu.Lock()
	lostConnection := o.wsConnected && !status.Connected
	o.wsConnected = status.Connected
	change := controlplane.StatusChange{Connected: status.Connected}
	for _, ch := range o.statuses {
		select {
		case ch <- change:
		default:
		}
	}
	o.eventsMu.Unlock()
	if lostConnection {
		// Older panels may close the socket without an explicit revocation.
		o.requestDiscovery(false)
	}
}

func (o *Orchestrator) registerNode(nodeID int, st chan<- controlplane.StatusChange) {
	o.eventsMu.Lock()
	o.statuses[nodeID] = st
	if o.wsConnected {
		// A socket can connect while the child is fetching its initial snapshot.
		select {
		case st <- controlplane.StatusChange{Connected: true}:
		default:
		}
	}
	o.eventsMu.Unlock()
}

func (o *Orchestrator) unregisterNode(nodeID int) {
	o.eventsMu.Lock()
	delete(o.mailboxes, nodeID)
	delete(o.statuses, nodeID)
	delete(o.kernels, nodeID)
	o.eventsMu.Unlock()
}

func (o *Orchestrator) applyIntervals(bc panel.MachineBaseConfig) {
	o.pullInterval = time.Duration(bc.PullInterval) * time.Second
	if o.pullInterval < 30*time.Second {
		o.pullInterval = 60 * time.Second
	}
	o.pushInterval = time.Duration(bc.PushInterval) * time.Second
	if o.pushInterval < 10*time.Second {
		o.pushInterval = 60 * time.Second
	}
}

// ─── Virtual PushClient ─────────────────────────────────────────────────

// machineNodePush implements controlplane.PushClient for a single node
// backed by the shared machine WS connection. Events are routed by the
// WS mux directly to the Service's channels; this adapter only provides
// connectivity status and send capabilities.
type machineNodePush struct {
	nodeID int
	mux    *atomic.Pointer[panel.WSClient]
}

func (p *machineNodePush) Run(ctx context.Context) {
	// The shared WS mux pushes events into our channels; we just wait.
	<-ctx.Done()
}

func (p *machineNodePush) IsConnected() bool {
	ws := p.mux.Load()
	return ws != nil && ws.IsConnected()
}

func (p *machineNodePush) IsEnabled() bool { return p.mux.Load() != nil }

func (p *machineNodePush) SendDeviceReport(devices map[int][]string) {
	ws := p.mux.Load()
	if ws == nil {
		return
	}
	payload := map[string]interface{}{
		"node_id": p.nodeID,
	}
	// Flatten into the standard format with node_id wrapper.
	strDevices := make(map[string][]string, len(devices))
	for uid, ips := range devices {
		strDevices[fmt.Sprintf("%d", uid)] = ips
	}
	payload["devices"] = strDevices
	data, _ := json.Marshal(payload)
	ws.SendRaw(panel.WSEventReportDevices, data)
}
