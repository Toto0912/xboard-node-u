package singbox

import (
	"context"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/sagernet/sing-vmess/vless"
	singM "github.com/sagernet/sing/common/metadata"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestLiveVLESSRevocationLeavesOtherTCPAndUDPUsable(t *testing.T) {
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	go func() {
		for {
			c, e := tcp.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	udp := udpLoopback(t)
	go func() {
		var b [100]byte
		for {
			n, a, e := udp.ReadFromUDP(b[:])
			if e != nil {
				return
			}
			_, _ = udp.WriteToUDP(b[:n], a)
		}
	}()
	node := &model.NodeSpec{Protocol: "vless", Network: "tcp", ListenIP: "127.0.0.1", ServerPort: freeTestPort(t), CustomRoutes: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}}}
	users := []model.UserSpec{{ID: 3, UUID: "11111111-1111-4111-8111-111111111111"}, {ID: 4, UUID: "22222222-2222-4222-8222-222222222222"}}
	k := New(config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir(), LogLevel: "error"})
	if e := k.Start(node, users, kernel.TLSCert{}); e != nil {
		t.Fatal(e)
	}
	defer k.Stop()
	connect := func(user model.UserSpec, packet bool) net.Conn {
		t.Helper()
		raw, e := net.DialTimeout("tcp4", net.JoinHostPort(node.ListenIP, strconv.Itoa(node.ServerPort)), time.Second)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { raw.Close() })
		cl, e := vless.NewClient(user.UUID, "", nil)
		if e != nil {
			t.Fatal(e)
		}
		var c net.Conn
		if packet {
			c, e = cl.DialEarlyPacketConn(raw, singM.SocksaddrFromNet(udp.LocalAddr()))
		} else {
			c, e = cl.DialEarlyConn(raw, singM.SocksaddrFromNet(tcp.Addr()))
		}
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	roundtrip := func(c net.Conn) bool {
		if e := c.SetDeadline(time.Now().Add(time.Second)); e != nil {
			return false
		}
		if _, e := c.Write([]byte("ok")); e != nil {
			return false
		}
		var b [2]byte
		_, e := io.ReadFull(c, b[:])
		return e == nil && string(b[:]) == "ok"
	}
	var opened [2][2]net.Conn
	for i, u := range users {
		for j := 0; j < 2; j++ {
			c := connect(u, j == 1)
			if !roundtrip(c) {
				t.Fatalf("initial user %d protocol %d", i, j)
			}
			opened[i][j] = c
		}
	}
	if _, _, err := k.UpdateUsers(users[1:]); err != nil {
		t.Fatal(err)
	}
	for j := 0; j < 2; j++ {
		if roundtrip(opened[0][j]) || !roundtrip(opened[1][j]) || roundtrip(connect(users[0], j == 1)) {
			t.Fatalf("revocation protocol %d", j)
		}
	}
	tr, _, _, err := k.GetUserTraffic(context.Background())
	if err != nil || tr[3][0] == 0 || tr[4][0] == 0 {
		t.Fatalf("traffic %v %v", tr, err)
	}
}
