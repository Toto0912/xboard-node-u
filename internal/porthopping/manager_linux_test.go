//go:build linux

package porthopping

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirewallLifecycleAndRollback(t *testing.T) {
	if os.Getenv("XBOARD_NODE_TEST_PORT_HOPPING") != "1" {
		t.Skip("requires isolated Linux NET_ADMIN test container")
	}
	for _, backend := range []string{"nft", "iptables"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			m := New("lifecycle-" + backend)
			m.dir = dir
			spec, _ := NewSpec("::", "60500-60504", 60500)
			pending := &record{Owner: m.owner, PID: os.Getpid(), Boot: bootIdentity(), Process: processIdentity(os.Getpid()), Backend: backend, Pending: spec}
			if err := save(dir, pending); err != nil {
				t.Fatal(err)
			}
			var listener net.PacketConn
			open := func(port string) error { var err error; listener, err = net.ListenPacket("udp", port); return err }
			closeSocket := func() {
				if listener != nil {
					listener.Close()
					listener = nil
				}
			}
			rollback := func() error { closeSocket(); return open("[::]:60500") }
			t.Cleanup(func() {
				closeSocket()
				if err := m.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := m.Apply(spec, func() error { return open("[::]:60500") }, nil); err != nil {
				t.Fatal(err)
			}
			if err := m.Apply(spec, func() error { return nil }, nil); err != nil {
				t.Fatal(err)
			}
			all, err := records(dir)
			if err != nil || len(all) != 1 || all[0].Pending != nil {
				t.Fatalf("invalid active state: %v %v", all, err)
			}
			other := New("conflicting")
			other.dir = dir
			conflict, _ := NewSpec("127.0.0.1", "60502-60508", 60502)
			activated := false
			if err := other.Apply(conflict, func() error { activated = true; return nil }, nil); err == nil || activated {
				t.Fatal("overlap was not rejected before activation")
			}
			next, _ := NewSpec("::", "60510-60514", 60510)
			if err := m.Apply(next, func() error { closeSocket(); return errors.New("activation failed") }, rollback); err == nil {
				t.Fatal("expected rollback")
			}
			all, _ = records(dir)
			if len(all) != 1 || !same(all[0].Active, spec) || listener == nil {
				t.Fatal("old node not restored")
			}
			if backend == "nft" {
				nft, err := exec.LookPath("nft")
				if err != nil {
					t.Fatal(err)
				}
				shim := t.TempDir()
				script := "#!/bin/sh\nif [ \"$1\" = \"-f\" ]; then\n input=$(cat)\n case \"$input\" in *60511-60514*) exit 42;; esac\n printf '%s\\n' \"$input\" | \"" + nft + "\" \"$@\"\nelse\n exec \"" + nft + "\" \"$@\"\nfi\n"
				if err := os.WriteFile(filepath.Join(shim, "nft"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", shim+":"+os.Getenv("PATH"))
				if err := m.Apply(next, func() error { closeSocket(); return open("[::]:60510") }, rollback); err == nil || !strings.Contains(err.Error(), "42") {
					t.Fatalf("expected rule failure: %v", err)
				}
				all, _ = records(dir)
				if len(all) != 1 || !same(all[0].Active, spec) || listener == nil {
					t.Fatal("rule failure did not restore old node")
				}
			}
			if err := m.Apply(nil, func() error { closeSocket(); return nil }, rollback); err != nil {
				t.Fatal(err)
			}
			all, _ = records(dir)
			if len(all) != 0 {
				t.Fatal("disabled range retained state")
			}
		})
	}
}

func TestStaleCleanupLeavesLiveNodesAlone(t *testing.T) {
	if os.Getenv("XBOARD_NODE_TEST_PORT_HOPPING") != "1" {
		t.Skip("requires isolated Linux NET_ADMIN test container")
	}
	dir := t.TempDir()
	t.Setenv("XBOARD_NODE_PORT_HOPPING_STATE_DIR", dir)
	live := New("live")
	dead := New("dead")
	liveSpec, _ := NewSpec("::", "60600-60604", 60600)
	deadSpec, _ := NewSpec("::", "60610-60614", 60610)
	for m, s := range map[*Manager]*Spec{live: liveSpec, dead: deadSpec} {
		if err := m.Apply(s, func() error { return nil }, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	all, _ := records(dir)
	for _, r := range all {
		if r.Owner == dead.owner {
			r.PID = 99999999
			r.Process = "not-running"
			if err := save(dir, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := CleanupStale(); err != nil {
		t.Fatal(err)
	}
	all, _ = records(dir)
	if len(all) != 1 || all[0].Owner != live.owner {
		t.Fatal("cleanup removed a live owner's rules")
	}
	if err := CleanupStopped(); err == nil {
		t.Fatal("uninstall cleanup accepted a live owner")
	}
}

func TestSocketConflictsRespectListenAddress(t *testing.T) {
	if os.Getenv("XBOARD_NODE_TEST_PORT_HOPPING") != "1" {
		t.Skip("requires isolated Linux NET_ADMIN test container")
	}
	listener, err := net.ListenPacket("udp4", "127.0.0.1:60803")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	m := New("socket-conflict")
	m.dir = t.TempDir()
	wide, _ := NewSpec("0.0.0.0", "60800-60805", 60800)
	activated := false
	if err := m.Apply(wide, func() error { activated = true; return nil }, nil); err == nil || activated {
		t.Fatal("occupied UDP port not rejected")
	}
	scoped, _ := NewSpec("127.0.0.2", "60800-60805", 60800)
	if err := m.Apply(scoped, func() error { return nil }, nil); err != nil {
		t.Fatal("unrelated bind address incorrectly rejected:", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	v6, err := net.ListenPacket("udp6", "[::1]:60803")
	if err != nil {
		t.Fatal(err)
	}
	defer v6.Close()
	dual, _ := NewSpec("::", "60800-60805", 60800)
	if err := m.Apply(dual, func() error { return nil }, nil); err == nil {
		t.Fatal("IPv6 UDP conflict not rejected")
	}
}
