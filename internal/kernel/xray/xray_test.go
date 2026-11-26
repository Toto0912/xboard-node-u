package xray

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	appstats "github.com/xtls/xray-core/app/stats"
	xrayUUID "github.com/xtls/xray-core/common/uuid"
	xrayCore "github.com/xtls/xray-core/core"
	featurebandwidth "github.com/xtls/xray-core/features/bandwidth"
	xraystats "github.com/xtls/xray-core/features/stats"
	"golang.org/x/time/rate"
)

func newStatsManager(t *testing.T) xraystats.Manager {
	t.Helper()
	mgr, err := appstats.NewManager(context.Background(), &appstats.Config{})
	if err != nil {
		t.Fatalf("stats.NewManager() error = %v", err)
	}
	return mgr
}

func mustRegisterCounter(t *testing.T, mgr xraystats.Manager, name string) xraystats.Counter {
	t.Helper()
	counter, err := mgr.RegisterCounter(name)
	if err != nil {
		t.Fatalf("RegisterCounter(%q) error = %v", name, err)
	}
	return counter
}

func counterName(userID int, direction string) string {
	return fmt.Sprintf("user>>>%s>>>traffic>>>%s", userEmail(userID), direction)
}

func newStatsBackedXray(t *testing.T, users []model.UserSpec, ld *LimitDispatcher) (*Xray, xraystats.Manager) {
	t.Helper()
	mgr := newStatsManager(t)
	inst := new(xrayCore.Instance)
	if err := inst.AddFeature(mgr); err != nil {
		t.Fatalf("Instance.AddFeature(stats) error = %v", err)
	}
	x := New(config.KernelConfig{Type: "xray"})
	x.instance = inst
	x.limitDispatcher = ld
	x.users = users
	x.cumTraffic = make(map[int][2]int64)
	x.running.Store(true)
	return x, mgr
}

func TestXrayGetUserTrafficUsesBuiltInStatsAndDispatcherState(t *testing.T) {
	users := []model.UserSpec{
		{ID: 1, UUID: "uuid-1"},
		{ID: 2, UUID: "uuid-2"},
	}

	ld := newTestDispatcher()
	email1 := userEmail(1)
	email2 := userEmail(2)
	ld.UpdateLimits(map[string]int{email1: 1, email2: 2}, nil, nil)
	ld.mu.Lock()
	ld.limitedIPs[email1] = map[string]int{"1.1.1.1": 1}
	ld.mu.Unlock()
	ref := &atomic.Int64{}
	ref.Store(1)
	ic := &ipCounter{}
	ic.ips.Store("2.2.2.2", ref)
	ld.unlimitedIPs.Store(email2, ic)
	ld.connCount.Store(7)

	x, mgr := newStatsBackedXray(t, users, ld)
	up1 := mustRegisterCounter(t, mgr, counterName(1, "uplink"))
	down1 := mustRegisterCounter(t, mgr, counterName(1, "downlink"))
	up2 := mustRegisterCounter(t, mgr, counterName(2, "uplink"))
	down2 := mustRegisterCounter(t, mgr, counterName(2, "downlink"))
	up1.Add(100)
	down1.Add(200)
	up2.Add(50)
	down2.Add(80)

	traffic, aliveIPs, connCount, err := x.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatalf("GetUserTraffic() error = %v", err)
	}
	if connCount != 7 {
		t.Fatalf("connCount = %d, want 7", connCount)
	}
	if got := traffic[1]; got != [2]int64{100, 200} {
		t.Fatalf("traffic[1] = %v, want [100 200]", got)
	}
	if got := traffic[2]; got != [2]int64{50, 80} {
		t.Fatalf("traffic[2] = %v, want [50 80]", got)
	}
	if !aliveIPs[1]["1.1.1.1"] {
		t.Fatal("expected limited-user IP to be reported")
	}
	if !aliveIPs[2]["2.2.2.2"] {
		t.Fatal("expected unlimited-user IP to be reported")
	}

	up1.Add(30)
	down1.Add(40)
	traffic, _, _, err = x.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatalf("second GetUserTraffic() error = %v", err)
	}
	if got := traffic[1]; got != [2]int64{130, 240} {
		t.Fatalf("traffic[1] after second poll = %v, want [130 240]", got)
	}
	if got := traffic[2]; got != [2]int64{50, 80} {
		t.Fatalf("traffic[2] after second poll = %v, want [50 80]", got)
	}
}

func TestXrayUpdateDispatcherLimitsPropagatesDeviceMetadata(t *testing.T) {
	x := New(config.KernelConfig{Type: "xray"})
	ld := newTestDispatcher()
	x.limitDispatcher = ld

	users := []model.UserSpec{
		{ID: 7, UUID: "uuid-7", DeviceLimit: 2, SpeedLimit: 12},
		{ID: 9, UUID: "uuid-9", DeviceLimit: 0, SpeedLimit: 0},
	}

	x.updateDispatcherLimits(users)

	if got := ld.deviceLimits[userEmail(7)]; got != 2 {
		t.Fatalf("deviceLimits[email] = %d, want 2", got)
	}
	if got := ld.deviceLimits["uuid-7"]; got != 2 {
		t.Fatalf("deviceLimits[uuid] = %d, want 2", got)
	}
	if _, ok := ld.deviceLimits[userEmail(9)]; ok {
		t.Fatal("unexpected device limit entry for unlimited user")
	}
}

func TestXraySetSpeedLimitFuncUsesPatchedCorePath(t *testing.T) {
	x := New(config.KernelConfig{Type: "xray"})
	called := false

	x.SetSpeedLimitFunc(func(string) *rate.Limiter {
		called = true
		return rate.NewLimiter(rate.Limit(1), 1)
	})

	if called {
		t.Fatal("patched xray path should not consult external speed-limit callbacks")
	}

	mu, err := toMemoryUser("vless", &model.NodeSpec{Protocol: "vless"}, model.UserSpec{ID: 1, UUID: "11111111-1111-1111-1111-111111111111", SpeedLimit: 8})
	if err != nil {
		t.Fatalf("toMemoryUser() error = %v", err)
	}
	if mu.Level != 0 {
		t.Fatalf("MemoryUser.Level = %d, want 0", mu.Level)
	}
}


func TestXrayCapabilities(t *testing.T) {
	x := New(config.KernelConfig{Type: "xray"})
	caps := x.Capabilities()
	if !caps.PerUserSpeedLimit || !caps.DeviceLimit || !caps.BuiltInTrafficStats || !caps.AliveIPTracking {
		t.Fatalf("unexpected positive xray capabilities: %+v", caps)
	}
	if caps.ForceCloseConnection || caps.ForceCloseUser {
		t.Fatalf("unexpected force-close xray capabilities: %+v", caps)
	}
}


func TestXrayUpdateBandwidthLimitsWritesPatchedCoreFeature(t *testing.T) {
	inst := new(xrayCore.Instance)
	bm := featurebandwidth.New()
	if err := inst.AddFeature(bm); err != nil {
		t.Fatalf("Instance.AddFeature(bandwidth) error = %v", err)
	}
	x := New(config.KernelConfig{Type: "xray"})
	x.instance = inst
	x.updateBandwidthLimits([]model.UserSpec{{ID: 1, UUID: "uuid-1", SpeedLimit: 8}})
	lim := bm.GetUserLimiter(userEmail(1))
	if lim == nil {
		t.Fatal("expected patched bandwidth feature to receive user limiter")
	}
}


func TestXrayUpdateBandwidthLimitsUsesSpeedLimitFunc(t *testing.T) {
	inst := new(xrayCore.Instance)
	bm := featurebandwidth.New()
	if err := inst.AddFeature(bm); err != nil {
		t.Fatalf("Instance.AddFeature(bandwidth) error = %v", err)
	}
	x := New(config.KernelConfig{Type: "xray"})
	x.instance = inst
	x.users = []model.UserSpec{{ID: 1, UUID: "uuid-1", SpeedLimit: 8}}
	shared := rate.NewLimiter(7, 7)
	x.SetSpeedLimitFunc(func(uuid string) *rate.Limiter {
		if uuid != "uuid-1" {
			t.Fatalf("unexpected uuid: %s", uuid)
		}
		return shared
	})
	lim := bm.GetUserLimiter(userEmail(1))
	if lim != shared {
		t.Fatal("expected patched bandwidth feature to consume shared limiter from callback")
	}
}


func TestXrayUpdateBandwidthLimitsFallsBackToUserSpeed(t *testing.T) {
	inst := new(xrayCore.Instance)
	bm := featurebandwidth.New()
	if err := inst.AddFeature(bm); err != nil {
		t.Fatalf("Instance.AddFeature(bandwidth) error = %v", err)
	}
	x := New(config.KernelConfig{Type: "xray"})
	x.instance = inst
	x.users = []model.UserSpec{{ID: 2, UUID: "uuid-2", SpeedLimit: 16}}
	x.updateBandwidthLimits(nil)
	if bm.GetUserLimiter(userEmail(2)) == nil {
		t.Fatal("expected fallback limiter derived from user speed")
	}
}


func TestXrayUpdateUsersLimitOnlyRefreshesDispatcherAndBandwidth(t *testing.T) {
	x := New(config.KernelConfig{Type: "xray"})
	ld := newTestDispatcher()
	inst := new(xrayCore.Instance)
	bm := featurebandwidth.New()
	if err := inst.AddFeature(bm); err != nil {
		t.Fatalf("Instance.AddFeature(bandwidth) error = %v", err)
	}
	x.instance = inst
	x.limitDispatcher = ld
	x.users = []model.UserSpec{{ID: 1, UUID: "uuid-1", DeviceLimit: 1, SpeedLimit: 8}}

	added, removed, err := x.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "uuid-1", DeviceLimit: 3, SpeedLimit: 16}})
	if err != nil {
		t.Fatalf("UpdateUsers() error = %v", err)
	}
	if added != 0 || removed != 0 {
		t.Fatalf("UpdateUsers() counts = (%d, %d), want (0, 0)", added, removed)
	}
	if got := ld.deviceLimits[userEmail(1)]; got != 3 {
		t.Fatalf("device limit after refresh = %d, want 3", got)
	}
	if bm.GetUserLimiter(userEmail(1)) == nil {
		t.Fatal("expected bandwidth limiter to be refreshed for unchanged user set")
	}
}

func TestXrayRemoveUsersStopsKernelWhenLastUserRemoved(t *testing.T) {
	x := New(config.KernelConfig{Type: "xray"})
	x.instance = new(xrayCore.Instance)
	ld := newTestDispatcher()
	x.limitDispatcher = ld
	x.users = []model.UserSpec{{ID: 1, UUID: "uuid-1"}}
	x.running.Store(true)

	removed, err := x.RemoveUsers([]model.UserSpec{{ID: 1, UUID: "uuid-1"}})
	if err != nil {
		t.Fatalf("RemoveUsers() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if x.IsRunning() {
		t.Fatal("expected xray to stop when last user is removed")
	}
}

func TestIsXrayUserNotFoundErr(t *testing.T) {
	if !isXrayUserNotFoundErr(fmt.Errorf("User u1@xboard.local not found")) {
		t.Fatal("expected 'not found' error to be treated as non-fatal")
	}
	if isXrayUserNotFoundErr(fmt.Errorf("context deadline exceeded")) {
		t.Fatal("expected timeout error NOT to be treated as user not found")
	}
}

func startLocalTCPEcho(t *testing.T) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo server: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(conn net.Conn) {
				defer wg.Done()
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	return port, func() {
		_ = ln.Close()
		wg.Wait()
	}
}

func reserveFreeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve tcp port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func buildVLESSv0Handshake(t *testing.T, idStr string, targetPort int, payload []byte) []byte {
	t.Helper()
	u, err := xrayUUID.ParseString(idStr)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", idStr, err)
	}
	var buf bytes.Buffer
	buf.WriteByte(0x00) // VLESS version 0
	buf.Write(u.Bytes())
	buf.WriteByte(0x00) // addons len
	buf.WriteByte(0x01) // command TCP
	var portBE [2]byte
	binary.BigEndian.PutUint16(portBE[:], uint16(targetPort))
	buf.Write(portBE[:])
	buf.WriteByte(0x01) // IPv4
	buf.Write(net.ParseIP("127.0.0.1").To4())
	buf.Write(payload)
	return buf.Bytes()
}

func readVLESSResponseHeader(t *testing.T, conn net.Conn) error {
	t.Helper()
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return err
	}
	if hdr[0] != 0x00 {
		return fmt.Errorf("unexpected VLESS response version: %d", hdr[0])
	}
	if hdr[1] > 0 {
		addons := make([]byte, int(hdr[1]))
		if _, err := io.ReadFull(conn, addons); err != nil {
			return err
		}
	}
	return nil
}

func TestXrayVLESSUUIDChangeKeepsOtherUserConnectionAlive(t *testing.T) {
	echoPort, stopEcho := startLocalTCPEcho(t)
	defer stopEcho()

	listenPort := reserveFreeTCPPort(t)
	uuid1 := "11111111-1111-1111-1111-111111111111"
	uuid2Old := "22222222-2222-2222-2222-222222222222"
	uuid2New := "33333333-3333-3333-3333-333333333333"

	nc := &model.NodeSpec{
		Protocol:   "vless",
		ListenIP:   "127.0.0.1",
		ServerPort: listenPort,
		Network:    "tcp",
		TLS:        0,
		CustomRoutes: []map[string]any{
			{
				"type":        "field",
				"ip":          []string{"127.0.0.1/32"},
				"outboundTag": "direct",
			},
		},
	}

	x := New(config.KernelConfig{
		Type:     "xray",
		LogLevel: "error",
		CustomOutbound: []map[string]any{
			{
				"tag":      "direct",
				"protocol": "freedom",
				"settings": map[string]any{
					"finalRules": []any{
						map[string]any{
							"action": "allow",
							"ip":     []string{"127.0.0.1/32"},
						},
					},
				},
			},
		},
	})
	if err := x.Start(nc, []model.UserSpec{
		{ID: 1, UUID: uuid1},
		{ID: 2, UUID: uuid2Old},
	}, kernel.TLSCert{}); err != nil {
		t.Fatalf("x.Start() error = %v", err)
	}
	defer x.Stop()

	x.mu.Lock()
	instBefore := x.instance
	x.mu.Unlock()

	// Open User 1's connection BEFORE User 2's UUID changes, and keep it open.
	conn1, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", listenPort), 3*time.Second)
	if err != nil {
		t.Fatalf("dial user1 connection: %v", err)
	}
	defer conn1.Close()
	_ = conn1.SetDeadline(time.Now().Add(5 * time.Second))

	msgBefore := []byte("before-change\n")
	if _, err := conn1.Write(buildVLESSv0Handshake(t, uuid1, echoPort, msgBefore)); err != nil {
		t.Fatalf("write user1 handshake: %v", err)
	}
	if err := readVLESSResponseHeader(t, conn1); err != nil {
		t.Fatalf("read user1 VLESS response header: %v", err)
	}
	gotBefore := make([]byte, len(msgBefore))
	if _, err := io.ReadFull(conn1, gotBefore); err != nil {
		t.Fatalf("read user1 echo before UUID change: %v", err)
	}
	if !bytes.Equal(gotBefore, msgBefore) {
		t.Fatalf("user1 before echo = %q, want %q", gotBefore, msgBefore)
	}

	// Change User 2's UUID via hot UpdateUsers.
	added, removed, err := x.UpdateUsers([]model.UserSpec{
		{ID: 1, UUID: uuid1},
		{ID: 2, UUID: uuid2New},
	})
	if err != nil {
		t.Fatalf("UpdateUsers(UUID change) error = %v", err)
	}
	if added != 1 || removed != 1 {
		t.Fatalf("UpdateUsers counts = (+%d, -%d), want (+1, -1)", added, removed)
	}

	x.mu.Lock()
	instAfter := x.instance
	x.mu.Unlock()
	if instAfter != instBefore {
		t.Fatal("expected xray instance NOT to restart on same-ID UUID change")
	}

	// Verify User 1's ALREADY-OPEN connection stays alive and still echoes.
	msgAfter := []byte("after-change\n")
	_ = conn1.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn1.Write(msgAfter); err != nil {
		t.Fatalf("write to user1 existing connection after UUID change: %v", err)
	}
	gotAfter := make([]byte, len(msgAfter))
	if _, err := io.ReadFull(conn1, gotAfter); err != nil {
		t.Fatalf("user1 existing connection broken after user2 UUID change: %v", err)
	}
	if !bytes.Equal(gotAfter, msgAfter) {
		t.Fatalf("user1 after echo = %q, want %q", gotAfter, msgAfter)
	}

	// Verify User 2's NEW UUID succeeds.
	connNew, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", listenPort), 3*time.Second)
	if err != nil {
		t.Fatalf("dial user2 new UUID connection: %v", err)
	}
	defer connNew.Close()
	_ = connNew.SetDeadline(time.Now().Add(5 * time.Second))
	msgNew := []byte("new-uuid-ok\n")
	if _, err := connNew.Write(buildVLESSv0Handshake(t, uuid2New, echoPort, msgNew)); err != nil {
		t.Fatalf("write user2 new handshake: %v", err)
	}
	if err := readVLESSResponseHeader(t, connNew); err != nil {
		t.Fatalf("read user2 new response header: %v", err)
	}
	gotNew := make([]byte, len(msgNew))
	if _, err := io.ReadFull(connNew, gotNew); err != nil {
		t.Fatalf("read user2 new echo: %v", err)
	}
	if !bytes.Equal(gotNew, msgNew) {
		t.Fatalf("user2 new echo = %q, want %q", gotNew, msgNew)
	}

	// Verify User 2's OLD UUID is rejected.
	connOld, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", listenPort), 3*time.Second)
	if err != nil {
		t.Fatalf("dial user2 old UUID connection: %v", err)
	}
	defer connOld.Close()
	_ = connOld.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = connOld.Write(buildVLESSv0Handshake(t, uuid2Old, echoPort, []byte("old-uuid-rejected\n")))
	var probe [32]byte
	n, readErr := connOld.Read(probe[:])
	if readErr == nil && n > 0 {
		t.Fatalf("expected old UUID to be rejected, but got %d bytes: %q", n, probe[:n])
	}
}
