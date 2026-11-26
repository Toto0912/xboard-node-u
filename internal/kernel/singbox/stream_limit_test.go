package singbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/time/rate"
)

func tcpLoopbackPair(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err = net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	server, err = listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	for _, conn := range []*net.TCPConn{client, server} {
		if err := conn.SetDeadline(time.Now().Add(6 * time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestLimitedSingCopyStreamsCompleteTCPDataAndHalfCloses(t *testing.T) {
	sender, source := tcpLoopbackPair(t)
	receiver, destination := tcpLoopbackPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"fixture": 1})
	tracker.SetSpeedLimitFunc(func(string) *rate.Limiter { return rate.NewLimiter(125000, 125000) })
	tracked := tracker.RoutedConnection(ctx, destination, testInboundContext("fixture", "127.0.0.1"), nil, nil)
	defer tracked.Close()
	// 300 kB exceeds two 1-Mbps bursts and must take at least 1.4 s with a full initial bucket.
	payload := bytes.Repeat([]byte("stream-data-"), 25000)
	copyResult := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := bufio.Copy(tracked, source)
		if err == nil {
			err = N.CloseWrite(tracked)
		}
		copyResult <- err
	}()
	if _, err := sender.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := sender.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(receiver)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("received %d/%d bytes, err=%v", len(got), len(payload), err)
	}
	if err := <-copyResult; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < time.Second || elapsed >= 5*time.Second {
		t.Fatalf("1-Mbps transfer of %d bytes took %s", len(payload), elapsed)
	}
	if _, err := receiver.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 5)
	if _, err := io.ReadFull(tracked, reply); err != nil || string(reply) != "reply" {
		t.Fatalf("reverse traffic after half-close: %q, %v", reply, err)
	}
	traffic, _, _ := tracker.GetUserTraffic()
	if traffic[1] != [2]int64{5, int64(len(payload))} {
		t.Fatalf("traffic after real copy = %v", traffic[1])
	}
}

type writeFuncConn struct {
	*testConn
	write func([]byte) (int, error)
}

func (c *writeFuncConn) Write(p []byte) (int, error) { return c.write(p) }

func TestLimitedLargeWriteHandlesChangingBurstAndPartialErrors(t *testing.T) {
	underlyingErr := errors.New("fixture write failed")
	for _, tc := range []struct {
		name string
		fail bool
		err  error
		want error
	}{
		{"complete", false, nil, nil},
		{"short write", true, nil, io.ErrShortWrite},
		{"partial error", true, underlyingErr, underlyingErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received bytes.Buffer
			lim := rate.NewLimiter(1e6, 8)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			first, resized := make(chan struct{}), make(chan struct{})
			go func() {
				select {
				case <-first:
					lim.SetBurst(3)
				case <-ctx.Done():
				}
				close(resized)
			}()
			conn := &writeFuncConn{testConn: &testConn{}, write: func(p []byte) (int, error) {
				if received.Len() == 0 {
					if len(p) != 8 {
						t.Fatalf("first chunk size = %d", len(p))
					}
					close(first)
					<-resized
				} else {
					if len(p) > 3 {
						t.Fatalf("chunk %d exceeds new burst", len(p))
					}
					if tc.fail {
						received.Write(p[:1])
						return 1, tc.err
					}
				}
				return received.Write(p)
			}}
			user := &userStats{}
			tracked := &trackedConn{Conn: conn, us: user, limiter: lim, ctx: ctx}
			payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
			n, err := tracked.Write(payload)
			want := len(payload)
			if tc.fail {
				want = 9
			}
			if n != want || !errors.Is(err, tc.want) || !bytes.Equal(received.Bytes(), payload[:want]) || user.download.Load() != int64(want) {
				t.Fatalf("write = %d, %v; received %q; counted %d", n, err, received.Bytes(), user.download.Load())
			}
		})
	}
}

func TestLimitedLargeWriteCancellationCountsOnlySentPrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &writeFuncConn{testConn: &testConn{}, write: func(p []byte) (int, error) {
		cancel()
		return len(p), nil
	}}
	user := &userStats{}
	tracked := &trackedConn{Conn: conn, us: user, limiter: rate.NewLimiter(1, 4), ctx: ctx}
	n, err := tracked.Write([]byte("abcdefghijkl"))
	if n != 4 || !errors.Is(err, context.Canceled) || user.download.Load() != 4 {
		t.Fatalf("cancelled write = %d, %v; counted %d", n, err, user.download.Load())
	}
}

func TestLimitedOversizePacketRemainsOneDatagram(t *testing.T) {
	relay, client := udpLoopback(t), udpLoopback(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"fixture": 1})
	tracker.SetSpeedLimitFunc(func(string) *rate.Limiter { return rate.NewLimiter(4096, 16) })
	tracked := tracker.RoutedPacketConnection(ctx, bufio.NewPacketConn(relay), testInboundContext("fixture", "127.0.0.1"), nil, nil)
	defer tracked.Close()
	payload := bytes.Repeat([]byte("xyz"), 40)
	n, err := bufio.CopyPacket(tracked, onePacket(payload, singM.SocksaddrFromNet(client.LocalAddr())))
	if n != int64(len(payload)) || !errors.Is(err, io.EOF) {
		t.Fatalf("download copy = %d, %v", n, err)
	}
	got := make([]byte, 512)
	size, _, err := client.ReadFromUDP(got)
	if err != nil || !bytes.Equal(got[:size], payload) {
		t.Fatalf("download datagram = %d bytes, %v", size, err)
	}
	if _, err := client.WriteToUDP(payload, relay.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	b := buf.NewPacket()
	defer b.Release()
	if _, err := tracked.ReadPacket(b); err != nil || !bytes.Equal(b.Bytes(), payload) {
		t.Fatalf("upload datagram = %d bytes, %v", b.Len(), err)
	}
	traffic, _, _ := tracker.GetUserTraffic()
	if want := int64(len(payload)); traffic[1] != [2]int64{want, want} {
		t.Fatalf("packet traffic = %v", traffic[1])
	}
}
