package singbox

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/route"
	"github.com/sagernet/sing/common/bufio"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// Pause a successful UDP write before the copy loop can publish its byte count.
type drainPacketSocket struct {
	net.PacketConn
	wrote, resume chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
}

func (c *drainPacketSocket) WriteTo(b []byte, dest net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(b, dest)
	if c.wrote != nil && err == nil {
		close(c.wrote)
		<-c.resume
	}
	return n, err
}

func (c *drainPacketSocket) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.PacketConn.Close()
}

type drainPacketDialer struct {
	N.Dialer
	socket net.PacketConn
}

func (d drainPacketDialer) ListenPacket(context.Context, singM.Socksaddr) (net.PacketConn, error) {
	return d.socket, nil
}

func TestPacketSessionCloseWaitsForLastAccountingCallback(t *testing.T) {
	for direction, name := range []string{"upload", "download"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			inbound := &drainPacketSocket{PacketConn: udpLoopback(t), closed: make(chan struct{})}
			outbound := &drainPacketSocket{PacketConn: udpLoopback(t), closed: make(chan struct{})}
			sender := udpLoopback(t)
			reader, writer := inbound, outbound
			if direction == 1 {
				reader, writer = outbound, inbound
			}
			writer.wrote, writer.resume = make(chan struct{}), make(chan struct{})
			resume := sync.OnceFunc(func() { close(writer.resume) })
			defer resume()

			metadata := testInboundContext("fixture", "127.0.0.1")
			metadata.Destination = singM.SocksaddrFromNet(sender.LocalAddr())
			tracker := NewConnTracker(0)
			tracker.SetUserMap(map[string]int{"fixture": 1})
			tracked := tracker.RoutedPacketConnection(ctx, bufio.NewPacketConn(inbound), metadata, nil, nil)
			reported := make(chan [2]int64, 1)
			manager := route.NewConnectionManager(ctx, log.NewNOPFactory().Logger())
			manager.NewPacketConnection(ctx, drainPacketDialer{socket: outbound}, tracked, metadata, func(error) {
				traffic, _, _ := tracker.GetUserTraffic()
				reported <- traffic[1]
			})
			payload := []byte("final datagram")
			if _, err := sender.WriteTo(payload, reader.LocalAddr()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-writer.wrote:
			case <-time.After(3 * time.Second):
				t.Fatal("copy did not deliver the datagram")
			}

			// Wake the opposite reader while this direction is still inside WriteTo.
			// Its copy loop closes the wrapper once it has handled the read error.
			if err := writer.PacketConn.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-writer.closed:
			case <-time.After(3 * time.Second):
				t.Fatal("opposite copy did not exit")
			}
			resume()
			var want [2]int64
			want[direction] = int64(len(payload))
			select {
			case got := <-reported:
				if got != want {
					t.Fatalf("traffic when the session completed = %v, want %v", got, want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("session completion was never reported")
			}
		})
	}
}
