package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-vmess/vless"
	M "github.com/sagernet/sing/common/metadata"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

// The kernel has already installed the candidate. Delay its acknowledgement
// so real connections can exercise the restrictive policy during that window.
type delayedUserUpdate struct {
	kernel.Kernel
	entered chan struct{}
	release chan struct{}
	fail    atomic.Bool
}

func (k *delayedUserUpdate) UpdateUsers(users []model.UserSpec) (int, int, error) {
	a, r, err := k.Kernel.UpdateUsers(users)
	close(k.entered)
	<-k.release
	if k.fail.Load() {
		return a, r, errors.New("test update rejected")
	}
	return a, r, err
}
func (k *delayedUserUpdate) Start(n *model.NodeSpec, u []model.UserSpec, c kernel.TLSCert) error {
	if k.fail.Load() {
		return errors.New("test restart rejected")
	}
	return k.Kernel.Start(n, u, c)
}

func TestManagedPoliciesWaitForKernelAcknowledgement(t *testing.T) {
	for _, kind := range []string{"singbox", "xray"} {
		for _, failure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail=%t", kind, failure), func(t *testing.T) {
				listen := func() net.Listener {
					t.Helper()
					l, err := net.Listen("tcp4", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					return l
				}
				echo := listen()
				defer echo.Close()
				go func() {
					for {
						c, e := echo.Accept()
						if e != nil {
							return
						}
						go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
					}
				}()
				port := listen()
				addr := port.Addr().String()
				nc := &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", Network: "tcp", ServerPort: port.Addr().(*net.TCPAddr).Port}
				port.Close()
				a := model.UserSpec{ID: 1, UUID: "11111111-1111-4111-8111-111111111111", SpeedLimit: 1, DeviceLimit: 1}
				b := model.UserSpec{ID: 2, UUID: "22222222-2222-4222-8222-222222222222", SpeedLimit: 2}
				cfg := &config.Config{Kernel: config.KernelConfig{Type: kind, ConfigDir: t.TempDir(), LogLevel: "error"}}
				if kind == "xray" {
					nc.CustomRoutes = []map[string]any{{"type": "field", "ip": []string{"127.0.0.1/32"}, "outboundTag": "direct"}}
					cfg.Kernel.CustomOutbound = []map[string]any{{"tag": "direct", "protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}}
				} else {
					nc.CustomRoutes = []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}}
				}
				s := newService(cfg, nil)
				s.source = &zeroUserCertControlPlane{bootstrap: controlplane.Bootstrap{Config: nc, Users: []model.UserSpec{a}}}
				if err := s.initialSetup(context.Background()); err != nil {
					t.Fatal(err)
				}
				defer s.stopKernel()
				open := func(user, ip string) net.Conn {
					d := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}
					raw, e := d.Dial("tcp4", addr)
					if e != nil {
						return nil
					}
					raw.SetDeadline(time.Now().Add(2 * time.Second))
					client, e := vless.NewClient(user, "", nil)
					if e != nil {
						t.Fatal(e)
					}
					conn, e := client.DialConn(raw, M.ParseSocksaddr(echo.Addr().String()))
					if e == nil {
						_, e = conn.Write([]byte("p"))
					}
					var data [1]byte
					if e == nil {
						_, e = io.ReadFull(conn, data[:])
					}
					if e != nil || data[0] != 'p' {
						raw.Close()
						return nil
					}
					raw.SetDeadline(time.Time{})
					return conn
				}
				if !failure {
					verifyWireConfigProtection(t, s, nc, a, b, open)
				}
				existing := open(a.UUID, "127.0.0.1")
				if existing == nil {
					t.Fatal("original account unreachable")
				}
				defer existing.Close()
				if !failure {
					// The old listener must not grant b on a user-only poll or WS delta
					// after a transport mismatch. Only a paired recovery permits b.
					ctx := context.Background()
					s.handleWSEvent(ctx, controlplane.Event{Type: controlplane.EventConfigPending})
					users := []model.UserSpec{a, b}
					users[0].DeviceLimit = 4
					committed := false
					s.applyPullResult(ctx, pullResult{users: users, userHash: computeUserHash(users),
						configMutSeq: s.configMutSeq, commitUsers: func() { committed = true }})
					s.applyUserDelta(ctx, "add", []model.UserSpec{b})
					if conn := open(b.UUID, "127.0.0.1"); conn != nil {
						conn.Close()
						t.Fatal("old listener granted a pending account")
					}
					if limit, ok := s.userPolicy.DeviceLimit(a.ID, a.UUID); !ok || limit != 1 || committed {
						t.Fatal("old listener relaxed the quota or confirmed a partial list")
					}
					s.applyPullResult(ctx, pullResult{configConfirmed: true, users: users, userHash: computeUserHash(users),
						configMutSeq: s.configMutSeq, userMutSeq: s.userMutSeq, commitUsers: func() { committed = true }})
					conn := open(b.UUID, "127.0.0.1")
					if conn == nil || !committed || s.configPending {
						t.Fatal("full recovery failed to grant b")
					}
					conn.Close()
					if !s.applyUserUpdate(ctx, []model.UserSpec{a}, computeUserHash([]model.UserSpec{a})) {
						t.Fatal("reset fixture")
					}
				}
				k := &delayedUserUpdate{Kernel: s.kernel, entered: make(chan struct{}), release: make(chan struct{})}
				s.kernel = k
				target := []model.UserSpec{a, b}
				target[0].SpeedLimit = 8
				target[0].DeviceLimit = 3
				done := make(chan bool, 1)
				go func() { done <- s.applyUserUpdate(context.Background(), target, computeUserHash(target)) }()
				<-k.entered
				check := func(user, ip string, want bool) {
					t.Helper()
					c := open(user, ip)
					if c != nil {
						c.Close()
					}
					if (c != nil) != want {
						t.Errorf("connection for test user %s from %s = %t, want %t", user[:1], ip, c != nil, want)
					}
				}
				check(a.UUID, "127.0.0.2", false)
				check(b.UUID, "127.0.0.1", false)
				if got := s.speedTracker.GetLimiter(a.UUID); got == nil || got.Limit() != 125000 {
					t.Error("existing rate expanded before confirmation")
				}
				// Drain more than an initial one-second burst while the old cap
				// still applies. Xray also keeps the pre-update stream open.
				payload := bytes.Repeat([]byte{'p'}, 192*1024)
				roundTrip := func(c net.Conn) time.Duration {
					t.Helper()
					if c == nil {
						t.Error("expected open stream")
						return 0
					}
					c.SetDeadline(time.Now().Add(8 * time.Second))
					defer c.SetDeadline(time.Time{})
					started := time.Now()
					if _, err := c.Write(payload); err != nil {
						t.Error(err)
						return 0
					}
					got := make([]byte, len(payload))
					if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, payload) {
						t.Errorf("echo did not match: %v", err)
						return 0
					}
					return time.Since(started)
				}
				if !failure {
					stream := existing
					if kind != "xray" {
						stream = open(a.UUID, "127.0.0.1")
						if stream != nil {
							defer stream.Close()
						}
					}
					elapsed := roundTrip(stream)
					t.Logf("unconfirmed 1 Mbps account: 192 KiB echo in %s", elapsed)
					if elapsed < 350*time.Millisecond {
						t.Error("early rate increase on active stream")
					}
				}
				k.fail.Store(failure)
				close(k.release)
				ok := <-done
				if ok == failure {
					t.Fatalf("apply=%v, failure=%v", ok, failure)
				}
				if failure {
					check(b.UUID, "127.0.0.1", false)
					if limit, valid := s.userPolicy.DeviceLimit(a.ID, a.UUID); !valid || limit != 1 {
						t.Fatal("failed update relaxed confirmed policy")
					}
					if _, valid := s.userPolicy.DeviceLimit(b.ID, b.UUID); valid {
						t.Fatal("failed update confirmed new account")
					}
					k.fail.Store(false)
					if !s.applyUserUpdate(context.Background(), target, computeUserHash(target)) {
						t.Fatal("retry failed")
					}
				}
				check(a.UUID, "127.0.0.2", true)
				check(b.UUID, "127.0.0.1", true)
				if !failure && kind == "xray" {
					t.Logf("confirmed 8 Mbps account, original connection: 192 KiB echo in %s", roundTrip(existing))
				}
				if got := s.speedTracker.GetLimiter(a.UUID); got == nil || got.Limit() != 1000000 {
					t.Error("confirmed rate did not advance")
				}
			})
		}
	}
}
