package singbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/sagernet/sing-vmess/vless"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	singM "github.com/sagernet/sing/common/metadata"
	"golang.org/x/time/rate"
)

type packetReaderFunc func(*buf.Buffer) (singM.Socksaddr, error)

func (f packetReaderFunc) ReadPacket(b *buf.Buffer) (singM.Socksaddr, error) { return f(b) }

type packetWriterFunc func(*buf.Buffer, singM.Socksaddr) error

func (f packetWriterFunc) WritePacket(b *buf.Buffer, addr singM.Socksaddr) error { return f(b, addr) }

func onePacket(payload []byte, destination singM.Socksaddr) packetReaderFunc {
	return func(b *buf.Buffer) (singM.Socksaddr, error) {
		if payload == nil {
			return singM.Socksaddr{}, io.EOF
		}
		_, err := b.Write(payload)
		payload = nil
		return destination, err
	}
}

func udpLoopback(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestSingCopyPacketPreservesBothTrafficDirections(t *testing.T) {
	for _, limited := range []bool{false, true} {
		name := "unlimited"
		if limited {
			name = "limited"
		}
		t.Run(name, func(t *testing.T) {
			relay, client := udpLoopback(t), udpLoopback(t)
			tracker := NewConnTracker(0)
			tracker.SetUserMap(map[string]int{"fixture": 1})
			if limited {
				tracker.SetSpeedLimitFunc(func(string) *rate.Limiter { return rate.NewLimiter(1e6, 1024) })
			}
			tracked := tracker.RoutedPacketConnection(context.Background(), bufio.NewPacketConn(relay),
				testInboundContext("fixture", "127.0.0.1"), nil, nil)
			defer tracked.Close()
			download := []byte("a longer download payload")
			destination := singM.SocksaddrFromNet(client.LocalAddr())
			n, err := bufio.CopyPacket(tracked, onePacket(download, destination))
			if n != int64(len(download)) || !errors.Is(err, io.EOF) {
				t.Fatalf("download copy returned %d, %v", n, err)
			}
			received := make([]byte, 256)
			size, _, err := client.ReadFromUDP(received)
			if err != nil || !bytes.Equal(received[:size], download) {
				t.Fatalf("download = %q, %v", received[:size], err)
			}
			upload := []byte("up")
			if _, err := client.WriteToUDP(upload, relay.LocalAddr().(*net.UDPAddr)); err != nil {
				t.Fatal(err)
			}
			writer := packetWriterFunc(func(b *buf.Buffer, addr singM.Socksaddr) error {
				defer b.Release()
				if !bytes.Equal(b.Bytes(), upload) || addr.Unwrap() != destination.Unwrap() {
					t.Fatalf("upload = %q from %v", b.Bytes(), addr)
				}
				// End the copy after this datagram; counters run after the successful write.
				return relay.Close()
			})
			if n, _ := bufio.CopyPacket(writer, tracked); n != int64(len(upload)) {
				t.Fatalf("upload copy returned %d", n)
			}
			traffic, _, _ := tracker.GetUserTraffic()
			if want := [2]int64{int64(len(upload)), int64(len(download))}; traffic[1] != want {
				t.Fatalf("traffic = %v, want %v", traffic[1], want)
			}
		})
	}
}

func TestSingCopyPacketLimitedDownloadStopsBeforeDeliveryOnCancel(t *testing.T) {
	relay, client := udpLoopback(t), udpLoopback(t)
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"fixture": 1})
	lim := rate.NewLimiter(1, 64)
	lim.AllowN(time.Now(), lim.Burst())
	tracker.SetSpeedLimitFunc(func(string) *rate.Limiter { return lim })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracked := tracker.RoutedPacketConnection(ctx, bufio.NewPacketConn(relay), testInboundContext("fixture", "127.0.0.1"), nil, nil)
	defer tracked.Close()
	read, result := make(chan struct{}), make(chan error, 1)
	source := onePacket([]byte("limited datagram"), singM.SocksaddrFromNet(client.LocalAddr()))
	go func() {
		_, err := bufio.CopyPacket(tracked, packetReaderFunc(func(b *buf.Buffer) (singM.Socksaddr, error) {
			addr, err := source(b)
			if err == nil {
				close(read)
			}
			return addr, err
		}))
		result <- err
	}()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("copy did not read a datagram")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled copy returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled copy did not finish")
	}
	client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := client.ReadFromUDP(make([]byte, 64)); err == nil {
		t.Fatal("delivered a datagram without speed tokens")
	}
	traffic, _, _ := tracker.GetUserTraffic()
	if traffic[1] != [2]int64{} {
		t.Fatalf("cancelled datagram was counted: %v", traffic[1])
	}
}

func TestVLESSUDPRealNodeAccountsAsymmetricReplies(t *testing.T) {
	echo := udpLoopback(t)
	user := testUsers[0]
	node := &model.NodeSpec{
		Protocol: "vless", Network: "tcp", ListenIP: "127.0.0.1", ServerPort: freeTestPort(t),
		CustomRoutes: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}},
	}
	core := New(config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir(), LogLevel: "error"})
	if err := core.Start(node, []model.UserSpec{user}, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer core.Stop()
	raw, err := net.DialTimeout("tcp4", net.JoinHostPort(node.ListenIP, strconv.Itoa(node.ServerPort)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	client, err := vless.NewClient(user.UUID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	udp, err := client.DialEarlyPacketConn(raw, singM.SocksaddrFromNet(echo.LocalAddr()))
	if err != nil {
		t.Fatal(err)
	}

	var want [2]int64
	for _, request := range []string{"up", "second request"} {
		deadline := time.Now().Add(3 * time.Second)
		echo.SetDeadline(deadline)
		raw.SetDeadline(deadline)
		if _, err := udp.Write([]byte(request)); err != nil {
			t.Fatal(err)
		}
		var data [256]byte
		n, peer, err := echo.ReadFromUDP(data[:])
		if err != nil || string(data[:n]) != request {
			t.Fatalf("UDP target received %q, %v", data[:n], err)
		}
		reply := "reply from local UDP target: " + request
		if _, err := echo.WriteToUDP([]byte(reply), peer); err != nil {
			t.Fatal(err)
		}
		n, err = udp.Read(data[:])
		if err != nil || string(data[:n]) != reply {
			t.Fatalf("VLESS client received %q, %v", data[:n], err)
		}
		want[0] += int64(len(request))
		want[1] += int64(len(reply))
	}
	// The receiving socket may wake just before the copy loop publishes its counter.
	for deadline := time.Now().Add(time.Second); ; {
		traffic, _, _, err := core.GetUserTraffic(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if traffic[user.ID] == want {
			t.Logf("VLESS UDP user %d tracked %d upload bytes and %d download bytes", user.ID, want[0], want[1])
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("actual node counters = %v, want %v", traffic[user.ID], want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
