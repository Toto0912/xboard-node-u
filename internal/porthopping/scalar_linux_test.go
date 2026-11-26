package porthopping

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScalarListenerToRange(t *testing.T) {
	if os.Getenv("XBOARD_NODE_TEST_PORT_HOPPING") != "1" {
		t.Skip("requires isolated Linux NET_ADMIN test container")
	}
	const base = 60910
	spec, _ := NewSpec("127.0.0.1", "60910-60913", base)
	m := New("scalar-to-range")
	m.dir = t.TempDir()
	open := func(port int) *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	var core *net.UDPConn
	if err := m.ApplyScalar(spec.Listen, base, func() error { core = open(base); return nil }, nil); err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	other := open(base + 2)
	defer other.Close()
	communicates := func(conn *net.UDPConn) {
		t.Helper()
		client, err := net.DialUDP("udp4", nil, conn.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err = client.Write([]byte("ok")); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 2)
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil || string(buf[:n]) != "ok" {
			t.Fatal(n, err)
		}
	}
	activated := false
	if err := m.Apply(spec, func() error { activated = true; return nil }, nil); err == nil || activated {
		t.Fatal("another node's listener was ignored")
	}
	communicates(core)
	communicates(other)
	other.Close()
	target := open(0)
	defer target.Close()
	out, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: base + 1}, target.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(spec, func() error { activated = true; return nil }, nil); err == nil || activated {
		t.Fatal("outbound socket was ignored")
	}
	out.Close()
	if err := m.Apply(spec, func() error { return nil }, nil); err != nil {
		t.Fatal("owned scalar listener blocked activation", err)
	}
	if present, err := records(m.dir); err != nil || len(present) != 1 || !same(present[0].Active, spec) {
		t.Fatal("active rule missing", err)
	}
	communicates(core)
	if err := m.Apply(nil, func() error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if remaining, err := records(m.dir); err != nil || len(remaining) != 0 {
		t.Fatal("stale range", err)
	}
	communicates(core)
}

// A peer in a second Docker network namespace sends real ingress packets
// through the NAT PREROUTING chain (loopback traffic would bypass it).
func TestPortRangeExternalPeer(t *testing.T) {
	peer := os.Getenv("XBOARD_NODE_TEST_PORT_HOPPING_PEER")
	if peer == "" {
		t.Skip("requires a private network and external UDP test peer")
	}
	const port = 60940
	m := New("peer-scalar")
	m.dir = t.TempDir()
	defer m.Close()
	start := func(port int, reply string) (func(), error) {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
		if err != nil {
			return nil, err
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 64)
			for {
				_, addr, err := conn.ReadFromUDP(buf)
				if err != nil {
					return
				}
				conn.WriteToUDP([]byte(reply), addr)
			}
		}()
		return func() { conn.Close(); <-done }, nil
	}
	var closeCore func()
	if err := m.TrackListener("0.0.0.0", port, func() error { var err error; closeCore, err = start(port, "target"); return err }); err != nil {
		t.Fatal(err)
	}
	defer closeCore()
	closeOther, err := start(port+3, "neighbor")
	if err != nil {
		t.Fatal(err)
	}
	defer closeOther()
	client := http.Client{Timeout: 3 * time.Second}
	probe := func(port int, want string) {
		t.Helper()
		resp, err := client.Get(fmt.Sprintf("%s/%d", peer, port))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || string(body) != want {
			t.Fatalf("port %d got %q want %q err=%v", port, body, want, err)
		}
	}
	probe(port, "target")
	probe(port+1, "silent")
	spec, _ := NewSpec("0.0.0.0", "60940-60942", port)
	if err := m.Apply(spec, func() error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	probe(port+1, "target")
	probe(port+2, "target")
	probe(port+3, "neighbor")
	if expected := os.Getenv("XBOARD_NODE_TEST_EXPECT_BACKEND"); expected != "" {
		all, err := records(m.dir)
		if err != nil || len(all) != 1 || all[0].Backend != expected {
			t.Fatalf("expected %s rule, got %v: %v", expected, all, err)
		}
	}
	child := func(want string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestScalarChildMatchesRange$")
		cmd.Env = append(os.Environ(), "XBOARD_HOP_SCALAR_TEST_DIR="+m.dir, "XBOARD_HOP_SCALAR_EXPECT="+want)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("scalar child %s: %v\n%s", want, err, output)
		}
	}
	child("blocked")
	probe(port+1, "target")
	probe(port+3, "neighbor")
	conflict, _ := NewSpec("0.0.0.0", "60940-60943", port)
	if err := m.Apply(conflict, func() error { return nil }, nil); err == nil {
		t.Fatal("overlap accepted")
	}
	probe(port+2, "target")
	probe(port+3, "neighbor")
	if err := m.Apply(nil, func() error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	probe(port, "target")
	probe(port+1, "silent")
	probe(port+3, "neighbor")
	child("allowed")
}

func TestScalarChildMatchesRange(t *testing.T) {
	dir := os.Getenv("XBOARD_HOP_SCALAR_TEST_DIR")
	if dir == "" {
		t.Skip("started by external peer test")
	}
	m := New("second-scalar-process")
	m.dir = dir
	var conn *net.UDPConn
	opened := false
	err := m.ApplyScalar("0.0.0.0", 60941, func() error {
		opened = true
		var err error
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: 60941})
		return err
	}, nil)
	if conn != nil {
		defer conn.Close()
	}
	if os.Getenv("XBOARD_HOP_SCALAR_EXPECT") == "blocked" {
		if err == nil || !strings.Contains(err.Error(), "conflict with node owner") || opened {
			t.Fatalf("scalar over active range: opened=%t err=%v", opened, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 64)
		if _, addr, err := conn.ReadFromUDP(buf); err == nil {
			_, _ = conn.WriteToUDP([]byte("second-scalar"), addr)
		}
	}()
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(os.Getenv("XBOARD_NODE_TEST_PORT_HOPPING_PEER") + "/60941")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || string(body) != "second-scalar" {
		t.Fatalf("independent scalar did not receive ingress: %q, %v", body, err)
	}
}

func TestScalarFirstRangeDuringActivation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	m := New("scalar")
	m.dir = dir
	start := func() error { return nil }
	if err := m.ApplyScalar("127.0.0.1", 60941, start, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scalar-only activation created state: %v", err)
	}
	scope, _ := NewSpec("0.0.0.0", "60940-60942", 60940)
	other := &record{Owner: New("range").owner, PID: os.Getpid(), Process: processIdentity(os.Getpid()), Boot: bootIdentity(), Backend: "nft", Pending: scope}
	rolledBack := false
	err := m.ApplyScalar("127.0.0.1", 60941, func() error {
		return locked(dir, func() error { return save(dir, other) })
	}, func() error { rolledBack = true; return nil })
	if err == nil || !rolledBack {
		t.Fatalf("first concurrent range: rollback=%t err=%v", rolledBack, err)
	}
	if err := m.ApplyScalar("127.0.0.1", 60944, start, nil); err != nil {
		t.Fatal("disjoint scalar", err)
	}
}

func TestUnprivilegedScalarWithoutStateDirectory(t *testing.T) {
	dir := os.Getenv("XBOARD_NODE_TEST_READONLY_STATE_DIR")
	if dir == "" {
		t.Skip("requires a read-only installation without a range owner")
	}
	m := New("ordinary")
	m.dir = dir
	var sock net.PacketConn
	if err := m.ApplyScalar("127.0.0.1", 60952, func() error {
		var err error
		sock, err = net.ListenPacket("udp4", "127.0.0.1:60952")
		return err
	}, nil); err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scalar-only installation created state: %v", err)
	}
}

func TestConcurrentFirstRangeAndScalarBind(t *testing.T) {
	if os.Getenv("XBOARD_NODE_TEST_PORT_HOPPING") != "1" {
		t.Skip("requires isolated Linux NET_ADMIN test container")
	}
	rangeOwner, scalar := New("first-range"), New("late-scalar")
	dir := filepath.Join(t.TempDir(), "state")
	rangeOwner.dir, scalar.dir = dir, dir
	scope, _ := NewSpec("0.0.0.0", "60960-60962", 60960)
	started, resume := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	var listener net.PacketConn
	go func() {
		result <- scalar.ApplyScalar("127.0.0.1", 60961, func() error {
			close(started)
			<-resume
			var err error
			listener, err = net.ListenPacket("udp4", "127.0.0.1:60961")
			return err
		}, func() error {
			if listener != nil {
				return listener.Close()
			}
			return nil
		})
	}()
	<-started
	var base net.PacketConn
	err := rangeOwner.Apply(scope, func() error {
		var err error
		base, err = net.ListenPacket("udp4", ":60960")
		return err
	}, nil)
	close(resume)
	scalarErr := <-result
	if base != nil {
		defer base.Close()
	}
	defer rangeOwner.Close()
	if err != nil || scalarErr == nil || !strings.Contains(scalarErr.Error(), "conflict with node owner") {
		t.Fatalf("range activation=%v, scalar activation=%v", err, scalarErr)
	}
	check, err := net.ListenPacket("udp4", "127.0.0.1:60961")
	if err != nil {
		t.Fatal("scalar socket survived rollback:", err)
	}
	check.Close()
}
