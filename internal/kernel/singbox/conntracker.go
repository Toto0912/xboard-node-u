package singbox

import (
	"context"
	"errors"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/time/rate"

	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/nlog"
)

// userStats holds per-user traffic counters and alive IP tracking.
// Traffic counters are lock-free atomics (read on every packet).
// IP tracking uses a lightweight mutex (only touched at connect/disconnect).
type userStats struct {
	upload   atomic.Int64
	download atomic.Int64

	mu        sync.RWMutex   // RWMutex for concurrent reads
	ips       map[string]int // sourceIP → refcount (number of active conns from that IP)
	connCount int            // total active connections
}

// removeConn unregisters a connection from sourceIP.
func (u *userStats) removeConn(sourceIP string) {
	u.mu.Lock()
	u.connCount--
	u.ips[sourceIP]--
	if u.ips[sourceIP] <= 0 {
		delete(u.ips, sourceIP)
	}
	u.mu.Unlock()
}

// distinctIPs returns the number of distinct IPs currently connected.
func (u *userStats) distinctIPs() int {
	u.mu.Lock()
	n := len(u.ips)
	u.mu.Unlock()
	return n
}

// aliveIPList returns the set of alive IPs as a boolean map (for reporting).
func (u *userStats) aliveIPList() map[string]bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.ips) == 0 {
		return nil
	}
	result := make(map[string]bool, len(u.ips))
	for ip := range u.ips {
		result[ip] = true
	}
	return result
}

// ─── ConnTracker ────────────────────────────────────────────────────────────

// ConnTracker provides per-user traffic counting, alive IP tracking, device
// limit gate-keeping, and per-user speed limiting for the sing-box kernel.
//
// Architecture: instead of maintaining a per-connection map (O(connections)),
// it maintains per-user atomic counters and IP refcount sets (O(users)).
// This means:
//   - GetUserTraffic is O(users), not O(connections)
//   - No Snapshot/pending buffer needed
//   - No per-connection map lock contention
//   - Bytes are counted directly into per-user atomics via CountFunc callbacks
//
// The per-connection wrapper (trackedConn) is still needed for:
//   - Hooking into sing's ReadCounter/WriteCounter for zero-copy byte counting
//   - Close callback to decrement IP refcounts
//   - Per-connection rate limit token accumulation (amortized WaitN)
type ConnTracker struct {
	usersMu    sync.RWMutex
	users      map[int]*userStats // userID → stats
	managedTag string
	loginUUID  map[string]string          // protocol-specific authenticated login to canonical UUID
	uuidMap    map[string]int             // UUID → userID (for lookup in RoutedConnection)
	connMap    map[string]ownedConnection // connID → admitted TCP or UDP session

	idCounter atomic.Int64

	// speedLimitFunc resolves a user UUID to a *rate.Limiter.
	speedLimitFunc atomic.Pointer[func(uuid string) *rate.Limiter]

	// deviceLimitFunc resolves a user UUID to their device limit.
	deviceLimitFunc atomic.Pointer[func(uuid string) (int, bool)]
	userPolicy      atomic.Pointer[kernel.UserPolicy]

	// Multi-node device state from panel
	globalDevices    map[int]map[string]bool // userID → IP → exists
	globalMu         sync.RWMutex
	globalLastUpdate time.Time
}

// NewConnTracker creates a tracker.
func NewConnTracker(_ int) *ConnTracker {
	return &ConnTracker{
		users:         make(map[int]*userStats),
		uuidMap:       make(map[string]int),
		connMap:       make(map[string]ownedConnection),
		globalDevices: make(map[int]map[string]bool),
	}
}

// SetSpeedLimitFunc configures the per-user speed limit lookup.
func (t *ConnTracker) SetSpeedLimitFunc(fn func(uuid string) *rate.Limiter) {
	t.speedLimitFunc.Store(&fn)
}

// SetDeviceLimitFunc configures the per-user device limit lookup for gate-keeping.
func (t *ConnTracker) SetDeviceLimitFunc(fn func(uuid string) (int, bool)) {
	t.deviceLimitFunc.Store(&fn)
}

// SetUserMap replaces the UUID→userID mapping and ensures per-user stats
// structs exist for all users. Old users that are no longer present keep
// their stats until their connections drain.
func (t *ConnTracker) SetUserMap(m map[string]int) {
	t.setUserMaps(m, nil, "")
}

type ownedConnection struct {
	uuid   string
	closer io.Closer
}

func (t *ConnTracker) setUserMaps(m map[string]int, names map[string]string, tag string) {
	t.usersMu.Lock()
	var revoked []io.Closer
	for _, session := range t.connMap {
		if _, was := t.uuidMap[session.uuid]; was {
			if id, ok := m[session.uuid]; !ok || id != t.uuidMap[session.uuid] {
				revoked = append(revoked, session.closer)
			}
		}
	}
	t.loginUUID = names
	t.managedTag = tag
	t.uuidMap = m
	for _, uid := range m {
		if _, ok := t.users[uid]; !ok {
			t.users[uid] = &userStats{ips: make(map[string]int)}
		}
	}
	t.usersMu.Unlock()
	for _, c := range revoked {
		_ = c.Close()
	}
}

// UpdateGlobalDevices syncs global device state from panel.
func (t *ConnTracker) UpdateGlobalDevices(users map[int][]string) {
	t.globalMu.Lock()
	t.globalDevices = make(map[int]map[string]bool, len(users))
	for uid, ips := range users {
		m := make(map[string]bool, len(ips))
		for _, ip := range ips {
			m[ip] = true
		}
		t.globalDevices[uid] = m
	}
	t.globalLastUpdate = time.Now()
	t.globalMu.Unlock()
	nlog.Core().Debug("global device state updated", "users", len(users))
}

// ClearGlobalDevices resets global device state (on WS disconnect).
func (t *ConnTracker) ClearGlobalDevices() {
	t.globalMu.Lock()
	t.globalDevices = make(map[int]map[string]bool)
	t.globalLastUpdate = time.Time{}
	t.globalMu.Unlock()
	nlog.Core().Debug("global device state cleared")
}

// ─── adapter.ConnectionTracker ──────────────────────────────────────────────

var _ adapter.ConnectionTracker = (*ConnTracker)(nil)

// Node inbounds use TCP/UDP connections; TUN flows have no node user identity.
func (t *ConnTracker) RoutedFlow(context.Context, adapter.InboundContext, adapter.Rule, adapter.Outbound) tun.FlowTracker {
	return nil
}

// RoutedConnection wraps a TCP conn to count bytes per-user, track IPs,
// and optionally rate-limit. Gate-keeps device limits at connection time.
func (t *ConnTracker) RoutedConnection(
	ctx context.Context, conn net.Conn,
	metadata adapter.InboundContext,
	_ adapter.Rule, _ adapter.Outbound,
) net.Conn {
	uuid := metadata.User
	sourceIP := metadata.Source.Addr.String()

	t.usersMu.RLock()
	if t.loginUUID != nil {
		uuid = t.loginUUID[uuid]
	}
	uid := t.uuidMap[uuid]
	us := t.users[uid]
	managed := metadata.User != "" && t.managedTag != "" && metadata.Inbound == t.managedTag
	stale := managed && us == nil
	t.usersMu.RUnlock()
	if stale {
		_ = conn.Close()
		return conn
	}

	if !t.registerConn(us, uid, uuid, sourceIP, managed) {
		nlog.Core().Debug("singbox: device limit exceeded", "userID", uid, "ip", sourceIP)
		conn.Close()
		return conn
	}

	connID := t.nextID()

	var lim *rate.Limiter
	if slf := t.speedLimitFunc.Load(); slf != nil {
		lim = (*slf)(uuid)
	}

	tracked := &trackedConn{
		Conn:     conn,
		tracker:  t,
		us:       us,
		userID:   uid,
		connID:   connID,
		sourceIP: sourceIP,
		limiter:  lim,
		ctx:      ctx,
	}
	if !t.recordConnection(connID, uuid, uid, tracked) {
		_ = tracked.Close()
	}
	if lim == nil {
		return &countedConn{tracked}
	}
	return tracked
}

// RoutedPacketConnection tracks UDP admission and session ownership.
func (t *ConnTracker) RoutedPacketConnection(
	ctx context.Context, conn N.PacketConn,
	metadata adapter.InboundContext,
	_ adapter.Rule, _ adapter.Outbound,
) N.PacketConn {
	uuid := metadata.User
	sourceIP := metadata.Source.Addr.String()

	t.usersMu.RLock()
	if t.loginUUID != nil {
		uuid = t.loginUUID[uuid]
	}
	uid := t.uuidMap[uuid]
	us := t.users[uid]
	managed := metadata.User != "" && t.managedTag != "" && metadata.Inbound == t.managedTag
	stale := managed && us == nil
	t.usersMu.RUnlock()
	if stale {
		_ = conn.Close()
		return conn
	}

	if !t.registerConn(us, uid, uuid, sourceIP, managed) {
		nlog.Core().Debug("singbox: device limit exceeded", "userID", uid, "ip", sourceIP)
		conn.Close()
		return conn
	}

	connID := t.nextID()

	var lim *rate.Limiter
	if slf := t.speedLimitFunc.Load(); slf != nil {
		lim = (*slf)(uuid)
	}

	tracked := &trackedPacketConn{
		PacketConn: conn,
		tracker:    t,
		us:         us,
		userID:     uid,
		connID:     connID,
		sourceIP:   sourceIP,
		limiter:    lim,
		ctx:        ctx,
	}
	if !t.recordConnection(connID, uuid, uid, tracked) {
		_ = tracked.Close()
	}
	if lim == nil {
		return &countedPacketConn{tracked}
	}
	return tracked
}

// registerConn reserves the local device slot before another admission checks it.
func (t *ConnTracker) registerConn(us *userStats, uid int, uuid, ip string, managed bool) bool {
	if us == nil {
		return true
	}
	var limit int
	if policy := t.userPolicy.Load(); managed && policy != nil {
		var allowed bool
		limit, allowed = policy.DeviceLimit(uid, uuid)
		if !allowed {
			return false
		}
	} else if get := t.deviceLimitFunc.Load(); get != nil {
		limit, _ = (*get)(uuid)
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	if t.checkDeviceGate(us, uid, ip, limit) {
		return false
	}
	us.connCount++
	us.ips[ip]++
	return true
}

// Caller holds the user's lock across admission and registration.
func (t *ConnTracker) checkDeviceGate(us *userStats, userID int, sourceIP string, limit int) bool {
	if us == nil || limit <= 0 {
		return false
	}

	localIPs := us.ips
	localCount := len(localIPs)

	// Already known locally
	if localIPs[sourceIP] > 0 {
		return false
	}

	// A live local device already occupies each local slot. Ranking the panel
	// snapshot below cannot evict a live local connection.
	if localCount >= limit {
		return true
	}

	// Check global state freshness
	t.globalMu.RLock()
	globalStale := time.Since(t.globalLastUpdate) > 60*time.Second
	globalIPs := t.globalDevices[userID]
	t.globalMu.RUnlock()

	// Stale or missing global state → local-only check
	if globalStale || globalIPs == nil {
		return false
	}

	// Known globally (from other node)
	if globalIPs[sourceIP] {
		return false
	}

	// Merge local + global
	allIPs := make(map[string]bool)
	for ip := range localIPs {
		allIPs[ip] = true
	}
	for ip := range globalIPs {
		allIPs[ip] = true
	}

	if len(allIPs) < limit {
		return false
	}

	// Over limit → lexicographic selection
	ipList := make([]string, 0, len(allIPs)+1)
	for ip := range allIPs {
		ipList = append(ipList, ip)
	}
	ipList = append(ipList, sourceIP)
	sort.Strings(ipList)

	for i := 0; i < limit && i < len(ipList); i++ {
		if ipList[i] == sourceIP {
			return false
		}
	}
	nlog.Core().Debug("device limit: total over limit, rejecting",
		"userID", userID, "ip", sourceIP, "totalIPs", len(allIPs), "limit", limit)
	return true
}

func (t *ConnTracker) nextID() string {
	return "sb-" + formatInt36(t.idCounter.Add(1))
}

func formatInt36(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var buf [13]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}

// ─── Public API ─────────────────────────────────────────────────────────────

// GetUserTraffic returns per-user cumulative traffic and alive IPs.
// This is O(users), not O(connections).
func (t *ConnTracker) GetUserTraffic() (traffic map[int][2]int64, aliveIPs map[int]map[string]bool, connCount int) {
	t.usersMu.RLock()
	traffic = make(map[int][2]int64, len(t.users))
	aliveIPs = make(map[int]map[string]bool, len(t.users))
	for uid, us := range t.users {
		up := us.upload.Load()
		down := us.download.Load()
		if up > 0 || down > 0 {
			traffic[uid] = [2]int64{up, down}
		}

		ips := us.aliveIPList()
		if ips != nil {
			aliveIPs[uid] = ips
		}

		us.mu.Lock()
		connCount += us.connCount
		us.mu.Unlock()
	}
	t.usersMu.RUnlock()
	return
}

func (t *ConnTracker) recordConnection(id, uuid string, uid int, c io.Closer) bool {
	t.usersMu.Lock()
	defer t.usersMu.Unlock()
	if uid != 0 && t.uuidMap[uuid] != uid {
		return false
	}
	t.connMap[id] = ownedConnection{uuid, c}
	return true
}

// CloseByID force-closes a connection by its ID.
func (t *ConnTracker) CloseByID(id string) bool {
	t.usersMu.RLock()
	conn, ok := t.connMap[id]
	t.usersMu.RUnlock()
	if !ok {
		return false
	}
	_ = conn.closer.Close()
	return true
}

// CloseByUUID force-closes ALL connections for a given user UUID.
func (t *ConnTracker) CloseByUUID(uuid string) int {

	t.usersMu.RLock()
	var sessions []io.Closer
	for _, s := range t.connMap {
		if s.uuid == uuid && uuid != "" {
			sessions = append(sessions, s.closer)
		}
	}
	t.usersMu.RUnlock()
	for _, c := range sessions {
		_ = c.Close()
	}
	return len(sessions)
}

// ActiveCount returns the total number of active connections.
func (t *ConnTracker) ActiveCount() int {
	t.usersMu.RLock()
	total := 0
	for _, us := range t.users {
		us.mu.Lock()
		total += us.connCount
		us.mu.Unlock()
	}
	t.usersMu.RUnlock()
	return total
}

// removeConnRef removes the connection reference from connMap on close.
func (t *ConnTracker) removeConnRef(connID string) {
	t.usersMu.Lock()
	delete(t.connMap, connID)
	t.usersMu.Unlock()
}

// ─── trackedConn (TCP) ──────────────────────────────────────────────────────

// reserveChunk waits for at most one burst. SetBurst may shrink the bucket
// between observing its size and reserving tokens; retry with the new size.
func reserveChunk(ctx context.Context, lim *rate.Limiter, remaining int) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		size := remaining
		if lim.Limit() != rate.Inf {
			burst := lim.Burst()
			if burst <= 0 {
				return 0, errors.New("rate limiter has no burst capacity")
			}
			size = min(size, burst)
		}
		reservation := lim.ReserveN(time.Now(), size)
		if !reservation.OK() {
			continue
		}
		if delay := reservation.Delay(); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				reservation.Cancel()
				return 0, ctx.Err()
			}
		}
		return size, nil
	}
}

func waitBytes(ctx context.Context, lim *rate.Limiter, size int) error {
	if lim == nil {
		return nil
	}
	for size > 0 {
		n, err := reserveChunk(ctx, lim, size)
		if err != nil {
			return err
		}
		size -= n
	}
	return nil
}

// RateLimitedWriter writes each chunk as its tokens become available.
type RateLimitedWriter struct {
	writer  io.Writer
	limiter *rate.Limiter
	ctx     context.Context
}

func (w *RateLimitedWriter) Write(b []byte) (int, error) {
	if w.limiter == nil || len(b) == 0 {
		n, err := w.writer.Write(b)
		if n < len(b) && err == nil {
			err = io.ErrShortWrite
		}
		return n, err
	}
	written := 0
	for written < len(b) {
		size, err := reserveChunk(w.ctx, w.limiter, len(b)-written)
		if err != nil {
			return written, err
		}
		n, err := w.writer.Write(b[written : written+size])
		written += n
		if err != nil {
			return written, err
		}
		if n != size {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

type RateLimitedReadCloser struct {
	reader  io.Reader
	limiter *rate.Limiter
	ctx     context.Context
}

func (r *RateLimitedReadCloser) Read(b []byte) (int, error) {
	if r.limiter != nil {
		// Return streaming data in affordable chunks even for callers with large buffers.
		if burst := r.limiter.Burst(); burst > 0 && len(b) > burst {
			b = b[:burst]
		}
	}
	n, err := r.reader.Read(b)
	if waitErr := waitBytes(r.ctx, r.limiter, n); waitErr != nil {
		return n, waitErr
	}
	return n, err
}

type trackedConn struct {
	net.Conn
	writeMu  sync.Mutex // keep chunks of concurrent limited writes together
	tracker  *ConnTracker
	us       *userStats // per-user stats (upload/download atomics + IP tracking)
	userID   int        // user ID for device tracking
	connID   string
	sourceIP string
	limiter  *rate.Limiter
	ctx      context.Context
	closed   atomic.Bool
}

func (c *trackedConn) Read(b []byte) (int, error) {
	reader := RateLimitedReadCloser{reader: c.Conn, limiter: c.limiter, ctx: c.ctx}
	n, err := reader.Read(b)
	if n > 0 && c.us != nil {
		c.us.upload.Add(int64(n))
	}
	return n, err
}

func (c *trackedConn) Write(b []byte) (int, error) {
	if c.limiter != nil {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
	}
	writer := RateLimitedWriter{writer: c.Conn, limiter: c.limiter, ctx: c.ctx}
	n, err := writer.Write(b)
	if n > 0 && c.us != nil {
		c.us.download.Add(int64(n))
	}
	return n, err
}

func (c *trackedConn) CloseWrite() error {
	writer, _ := N.UnwrapCountWriter(c.Conn, nil)
	if closer, ok := N.UnwrapWriter(writer).(N.WriteCloser); ok {
		return closer.CloseWrite()
	}
	return c.Close()
}

func (c *trackedConn) CloseRead() error { return N.CloseRead(c.Conn) }

func (c *trackedConn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	if c.us != nil {
		c.us.removeConn(c.sourceIP)
	}
	c.tracker.removeConnRef(c.connID)
	return c.Conn.Close()
}

func (c *trackedConn) Upstream() any { return c.Conn }

// Unlimited connections retain sing's optimized copy paths. Limited copies must
// pass through Read and Write so cancellation can stop I/O and large writes can stream.
type countedConn struct{ *trackedConn }

func (c *countedConn) UnwrapReader() (io.Reader, []N.CountFunc) {
	if c.us == nil {
		return c.Conn, nil
	}
	return c.Conn, []N.CountFunc{func(n int64) { c.us.upload.Add(n) }}
}

func (c *countedConn) UnwrapWriter() (io.Writer, []N.CountFunc) {
	if c.us == nil {
		return c.Conn, nil
	}
	return c.Conn, []N.CountFunc{func(n int64) { c.us.download.Add(n) }}
}

// ─── trackedPacketConn (UDP / QUIC) ─────────────────────────────────────────

type trackedPacketConn struct {
	N.PacketConn
	tracker  *ConnTracker
	us       *userStats
	userID   int
	connID   string
	sourceIP string
	limiter  *rate.Limiter
	ctx      context.Context
	closed   atomic.Bool
}

func (c *trackedPacketConn) ReadPacket(buffer *buf.Buffer) (singM.Socksaddr, error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if err != nil {
		return dest, err
	}
	if err := waitBytes(c.ctx, c.limiter, buffer.Len()); err != nil {
		return dest, err
	}
	if c.us != nil {
		c.us.upload.Add(int64(buffer.Len()))
	}
	return dest, nil
}

func (c *trackedPacketConn) WritePacket(buffer *buf.Buffer, dest singM.Socksaddr) error {
	size := buffer.Len()
	if err := waitBytes(c.ctx, c.limiter, size); err != nil {
		buffer.Release()
		return err
	}
	err := c.PacketConn.WritePacket(buffer, dest)
	if err == nil && c.us != nil {
		c.us.download.Add(int64(size))
	}
	return err
}

func (c *trackedPacketConn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	if c.us != nil {
		c.us.removeConn(c.sourceIP)
	}
	c.tracker.removeConnRef(c.connID)
	return c.PacketConn.Close()
}

func (c *trackedPacketConn) Upstream() any { return c.PacketConn }

// Only unlimited sessions can hand their packet I/O to sing's fast copy loop.
// Generic writer replacement runs before the packet counter hook in sing.
type countedPacketConn struct{ *trackedPacketConn }

func (c *countedPacketConn) UnwrapPacketReader() (N.PacketReader, []N.CountFunc) {
	if c.us == nil {
		return c.PacketConn, nil
	}
	return c.PacketConn, []N.CountFunc{func(n int64) { c.us.upload.Add(n) }}
}

func (c *countedPacketConn) UnwrapPacketWriter() (N.PacketWriter, []N.CountFunc) {
	if c.us == nil {
		return c.PacketConn, nil
	}
	return c.PacketConn, []N.CountFunc{func(n int64) { c.us.download.Add(n) }}
}
