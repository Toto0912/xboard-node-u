package xray

import (
	"context"
	"errors"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	xrayDispatcher "github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

var origDispatcherFactory common.ConfigCreator

// globalLimitDispatcher is set when the factory creates a LimitDispatcher.
// The Xray kernel reads it to configure limits and get connections.
var globalLimitDispatcher atomic.Pointer[LimitDispatcher]

func init() {
	var err error
	origDispatcherFactory, err = common.ReplaceConfigCreator((*xrayDispatcher.Config)(nil), limitDispatcherFactory)
	common.Must(err)
}

func limitDispatcherFactory(ctx context.Context, config interface{}) (interface{}, error) {
	orig, err := origDispatcherFactory(ctx, config)
	if err != nil {
		return nil, err
	}
	inner, ok := orig.(routing.Dispatcher)
	if !ok {
		return orig, nil
	}
	ld := &LimitDispatcher{
		inner:     orig,
		innerDisp: inner,
		activeIPs: make(map[string]map[string]int),
	}
	globalLimitDispatcher.Store(ld)
	nlog.Core().Debug("xray: limit dispatcher installed")
	return ld, nil
}

// LimitDispatcher wraps xray's DefaultDispatcher to enforce per-user
// admission checks before a request is dispatched into xray-core.
//
// It preserves the transport reader and tracks closure via the writer. Xray's
// mux/XUDP close path requires the original concrete *pipe.Reader to remain
// intact, so the dispatcher is limited to gate-keeping and safe connection
// lifecycle bookkeeping.
type LimitDispatcher struct {
	inner     interface{}        // original DefaultDispatcher (Feature + Dispatcher)
	innerDisp routing.Dispatcher // same object, typed as Dispatcher

	// Track all active TCP addresses, independently of the current limit.
	// Needs deterministic IP ordering for kick decisions.
	mu           sync.RWMutex
	activeIPs    map[string]map[string]int // email → sourceIP → refcount
	deviceLimits map[string]int            // email → max devices
	emailToUID   map[string]int            // email → panel user ID

	managedTag string
	userPolicy *atomic.Pointer[kernel.UserPolicy]
	connCount  atomic.Int64 // total active connections tracked by dispatcher
}

// ─── routing.Dispatcher ──────────────────────────────────────────────────────

func (d *LimitDispatcher) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	email, sourceIP, isTCP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return nil, err
	}

	link, err := d.innerDisp.Dispatch(ctx, dest)
	if err != nil {
		if email != "" && isTCP {
			d.delConn(email, sourceIP)
		}
		return nil, err
	}

	if email != "" {
		d.trackLink(link, email, sourceIP, isTCP)
	}
	return link, nil
}

func (d *LimitDispatcher) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	email, sourceIP, isTCP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return err
	}

	if email == "" {
		return d.innerDisp.DispatchLink(ctx, dest, link)
	}
	writer := d.trackLink(link, email, sourceIP, isTCP)
	if err := d.innerDisp.DispatchLink(ctx, dest, link); err != nil {
		writer.release()
		return err
	}
	return nil
}

// identifyAndCheck extracts user identity from the session context, enforces
// device limits, and returns the user's email, source IP, and TCP flag.
// Returns a non-nil error only when the connection should be rejected.
func (d *LimitDispatcher) identifyAndCheck(ctx context.Context, dest net.Destination) (email, sourceIP string, isTCP bool, err error) {
	si := session.InboundFromContext(ctx)
	if si == nil || si.User == nil || len(si.User.Email) == 0 {
		return "", "", false, nil
	}
	email = si.User.Email
	var policyLimit *int
	if si.Tag == d.managedTag && d.userPolicy != nil {
		if policy := d.userPolicy.Load(); policy != nil {
			idStr, ok := strings.CutPrefix(email, "user@")
			id, parseErr := strconv.Atoi(idStr)
			limit, allowed := policy.DeviceLimit(id, "")
			if !ok || parseErr != nil || !allowed {
				return "", "", false, errors.New("managed user is not active")
			}
			policyLimit = &limit
		}
	}
	if si.Source.Address == nil || !si.Source.Address.Family().IsIP() {
		d.mu.RLock()
		limit := d.deviceLimits[email]
		d.mu.RUnlock()
		if policyLimit != nil {
			limit = *policyLimit
		}
		if limit > 0 {
			return "", "", false, errors.New("source IP required for device limit")
		}
		return "", "", false, nil
	}
	sourceIP = si.Source.Address.IP().String()
	isTCP = dest.Network == net.Network_TCP

	if d.checkDeviceLimit(email, sourceIP, isTCP, policyLimit) {
		nlog.Core().Debug("xray: device limit exceeded", "email", email, "ip", sourceIP)
		return "", "", false, errors.New("device limit exceeded for " + email)
	}
	return email, sourceIP, isTCP, nil
}

// trackLink records connection lifecycle without mutating xray-core owned
// transport primitives. This keeps mux/XUDP compatible while still allowing
// the dispatcher to release device-limit state when the link closes.
func (d *LimitDispatcher) trackLink(link *transport.Link, email, sourceIP string, isTCP bool) *closeTrackingWriter {
	d.connCount.Add(1)

	onClose := func() {
		if isTCP {
			d.delConn(email, sourceIP)
		}
		d.connCount.Add(-1)
	}

	writer := &closeTrackingWriter{
		Writer:  link.Writer,
		onClose: onClose,
	}
	link.Writer = writer
	return writer
}

// ─── features.Feature (delegated) ───────────────────────────────────────────

func (d *LimitDispatcher) Type() interface{} { return routing.DispatcherType() }

func (d *LimitDispatcher) Start() error {
	if s, ok := d.inner.(interface{ Start() error }); ok {
		return s.Start()
	}
	return nil
}

func (d *LimitDispatcher) Close() error {
	if c, ok := d.inner.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ─── Limit management (called by Xray kernel) ──────────────────────────────

func (d *LimitDispatcher) UpdateLimits(emailToUID map[string]int, deviceLimits, _ map[string]int) {
	d.mu.Lock()
	d.emailToUID = emailToUID
	d.deviceLimits = deviceLimits
	d.mu.Unlock()

}

func (d *LimitDispatcher) ResetConns() {
	d.mu.Lock()
	d.activeIPs = make(map[string]map[string]int)
	d.mu.Unlock()

	d.connCount.Store(0)
}

// GetConnectionState returns dispatcher-tracked alive IPs and connection count.
// Traffic bytes are intentionally left to xray's built-in stats pipeline.
func (d *LimitDispatcher) GetConnectionState() (aliveIPs map[int]map[string]bool, connCount int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	emailToUID := d.emailToUID
	activeIPs := d.activeIPs

	aliveIPs = make(map[int]map[string]bool)

	// The maps are mutable: keep the read lock until the snapshot is complete.
	for email, ipsMap := range activeIPs {
		uid := emailToUID[email]
		if uid == 0 {
			continue
		}
		ipSet := make(map[string]bool, len(ipsMap))
		for ip := range ipsMap {
			ipSet[ip] = true
		}
		if len(ipSet) > 0 {
			aliveIPs[uid] = ipSet
		}
	}

	connCount = int(d.connCount.Load())
	return
}

// ─── Internal helpers ───────────────────────────────────────────────────────

// checkDeviceLimit enforces per-user device limits.
func (d *LimitDispatcher) checkDeviceLimit(email, sourceIP string, isTCP bool, override ...*int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	limit := d.deviceLimits[email]
	if len(override) > 0 && override[0] != nil {
		limit = *override[0]
	}
	ips := d.activeIPs[email]
	if ips == nil {
		if !isTCP {
			return false
		}
		ips = make(map[string]int)
		d.activeIPs[email] = ips
	}

	if ips[sourceIP] > 0 {
		if isTCP {
			ips[sourceIP]++
		}
		return false
	}

	if limit <= 0 || len(ips) < limit {
		if isTCP {
			ips[sourceIP]++
		}
		return false
	}

	// Over limit — deterministic: allow lowest IPs lexicographically.
	ipList := make([]string, 0, len(ips)+1)
	for ip := range ips {
		ipList = append(ipList, ip)
	}
	ipList = append(ipList, sourceIP)
	sort.Strings(ipList)

	for i := 0; i < limit && i < len(ipList); i++ {
		if ipList[i] == sourceIP {
			if isTCP {
				ips[sourceIP]++
			}
			return false
		}
	}
	return true
}

// delConn decrements the IP refcount when a connection closes.
func (d *LimitDispatcher) delConn(email, sourceIP string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ips, ok := d.activeIPs[email]; ok {
		ips[sourceIP]--
		if ips[sourceIP] <= 0 {
			delete(ips, sourceIP)
		}
		if len(ips) == 0 {
			delete(d.activeIPs, email)
		}
	}
}

type closeTrackingWriter struct {
	buf.Writer
	onClose func()
	closed  atomic.Bool
}

func (w *closeTrackingWriter) release() {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
}

func (w *closeTrackingWriter) Close() error {
	w.release()
	return common.Close(w.Writer)
}

func (w *closeTrackingWriter) Interrupt() {
	w.release()
	common.Interrupt(w.Writer)
}
