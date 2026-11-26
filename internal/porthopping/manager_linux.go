//go:build linux

package porthopping

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type record struct {
	Owner   string
	PID     int
	Boot    string
	Process string
	Backend string
	Active  *Spec
	Pending *Spec
}

// command never invokes a shell. nft input is built exclusively from validated
// addresses, numeric ports and internally generated ownership identifiers.
func command(input, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func processIdentity(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The process name may itself contain spaces or parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 || fields[0] == "Z" {
		return ""
	}
	return fields[19] // /proc stat field 22, process start time.
}

func bootIdentity() string {
	data, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(data))
}

func alive(r *record) bool {
	return r.PID > 0 && r.Boot == bootIdentity() && r.Process != "" && processIdentity(r.PID) == r.Process
}

func locked(dir string, fn func() error) error {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	return withFileLock(lock, syscall.LOCK_EX, fn)
}

func withFileLock(lock *os.File, mode int, fn func() error) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := syscall.Flock(int(lock.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) || time.Now().After(deadline) {
			return fmt.Errorf("port-hopping state lock: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func recordPath(dir, owner string) string { return filepath.Join(dir, owner+".json") }

func save(dir string, r *record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	file := recordPath(dir, r.Owner)
	if err := os.WriteFile(file+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(file+".tmp", file)
}

func records(dir string) ([]*record, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]*record, 0, len(files))
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, file.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var r record
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("invalid port-hopping state %s: %w", path, err)
		}
		if len(r.Owner) != 20 || strings.Trim(r.Owner, "0123456789abcdef") != "" || r.PID < 1 || r.Boot == "" || r.Process == "" ||
			filepath.Base(path) != r.Owner+".json" || (r.Backend != "nft" && r.Backend != "iptables") {
			return nil, fmt.Errorf("unrecognized port-hopping ownership record %s", path)
		}
		for _, s := range []*Spec{r.Active, r.Pending} {
			if s == nil {
				continue
			}
			if _, err := NewSpec(s.Listen, fmt.Sprintf("%d-%d", s.Start, s.End), s.Start); err != nil {
				return nil, fmt.Errorf("invalid port-hopping state %s: %w", path, err)
			}
		}
		out = append(out, &r)
	}
	return out, nil
}

func chooseBackend(spec *Spec) (string, error) {
	if _, err := command("", "nft", "list", "tables"); err == nil {
		probe := &record{Owner: "probe_" + strconv.Itoa(os.Getpid())}
		if _, err := command(nftScript(probe, spec, false), "nft", "-c", "-f", "-"); err == nil {
			return "nft", nil
		}
	}
	for _, binary := range families(spec) {
		if _, err := command("", binary, "-w", "5", "-t", "nat", "-S"); err != nil {
			return "", fmt.Errorf("HY2 automatic redirects need usable nftables or iptables with NET_ADMIN; configure manual forwarding with a scalar server port: %w", err)
		}
	}
	return "iptables", nil
}

func cleanup(dir string, r *record) error {
	if err := removeRules(r); err != nil {
		return err
	}
	err := os.Remove(recordPath(dir, r.Owner))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func cleanDead(dir string, all []*record) error {
	var errs []error
	for _, r := range all {
		if !alive(r) {
			if err := cleanup(dir, r); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func CleanupStale() error {
	dir := stateDirectory()
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return locked(dir, func() error {
		all, err := records(dir)
		if err != nil {
			return err
		}
		return cleanDead(dir, all)
	})
}

// Uninstall must preserve the cleanup helper if a node is still running.
func CleanupStopped() error {
	if err := CleanupStale(); err != nil {
		return err
	}
	dir := stateDirectory()
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return locked(dir, func() error {
		all, err := records(dir)
		if err != nil {
			return err
		}
		for _, r := range all {
			if alive(r) {
				return fmt.Errorf("HY2 owner %s is still running as PID %d; stop it before uninstalling", r.Owner, r.PID)
			}
		}
		return nil
	})
}

// Apply reserves the range before opening sockets. A crash at any later step
// leaves a durable ownership record for ExecStopPost or startup recovery.
func (m *Manager) Apply(spec *Spec, activate, rollback func() error) error {
	return m.apply(spec, nil, activate, rollback)
}

// ApplyScalar checks managed redirects without creating a state directory or
// running firewall commands when the node only needs a single UDP listener.
func (m *Manager) ApplyScalar(listen string, port int, activate, rollback func() error) error {
	scope := "::"
	if ip, err := netip.ParseAddr(listen); err == nil {
		scope = ip.Unmap().String()
	}
	scalar := &Spec{Listen: scope, Start: port, End: port}
	return m.apply(nil, scalar, func() error { return m.TrackListener(listen, port, activate) }, rollback)
}

func (m *Manager) apply(spec, scalar *Spec, activate, rollback func() error) error {
	if spec != nil {
		start := activate
		activate = func() error { return m.TrackListener(spec.Listen, spec.Start, start) }
	}
	if spec == nil {
		if _, err := os.Stat(recordPath(m.dir, m.owner)); errors.Is(err, os.ErrNotExist) {
			if scalar != nil {
				return m.activateScalar(scalar, activate, rollback)
			}
			return activateOnly(activate, rollback) // scalar ports require no privileges.
		}
	}
	return locked(m.dir, func() error {
		all, err := records(m.dir)
		if err != nil {
			return err
		}
		if err := cleanDead(m.dir, all); err != nil {
			return fmt.Errorf("recover stale HY2 rules: %w", err)
		}
		var previous *record
		requested := spec
		if scalar != nil {
			requested = scalar
		}
		for _, r := range all {
			if !alive(r) {
				continue
			}
			if r.Owner == m.owner {
				if r.PID != os.Getpid() {
					return fmt.Errorf("HY2 range owner is already running as PID %d", r.PID)
				}
				previous = r
				continue
			}
			if err := checkRangeOwner(requested, r); err != nil {
				return err
			}
		}
		if spec == nil && previous == nil {
			return activateOnly(activate, rollback)
		}
		if spec != nil {
			if err := m.checkSockets(spec); err != nil {
				return err
			}
		}
		if previous != nil && previous.Pending == nil && same(previous.Active, spec) {
			return activateOnly(activate, rollback) // no duplicate rules.
		}
		r := &record{Owner: m.owner, PID: os.Getpid(), Boot: bootIdentity(), Process: processIdentity(os.Getpid()), Pending: spec}
		if r.Boot == "" || r.Process == "" {
			return fmt.Errorf("cannot establish process ownership for HY2 redirects")
		}
		if previous != nil {
			r.Backend, r.Active = previous.Backend, previous.Active
		} else {
			r.Backend, err = chooseBackend(spec)
			if err != nil {
				return err
			}
		}
		if err := save(m.dir, r); err != nil {
			return err
		}
		restore := func(cause error) error {
			var failures []error
			failures = append(failures, cause)
			if rollback != nil {
				if previous != nil && previous.Active != nil {
					failures = append(failures, m.TrackListener(previous.Active.Listen, previous.Active.Start, rollback))
				} else {
					failures = append(failures, rollback())
				}
			}
			if previous != nil {
				if err := applyRules(r, previous.Active); err != nil {
					return errors.Join(append(failures, fmt.Errorf("restore old rules: %w", err))...)
				}
				failures = append(failures, save(m.dir, previous))
			} else {
				failures = append(failures, cleanup(m.dir, r))
			}
			return errors.Join(failures...)
		}
		if err := activate(); err != nil {
			return restore(err)
		}
		if err := applyRules(r, spec); err != nil {
			return restore(fmt.Errorf("apply HY2 UDP redirects: %w", err))
		}
		if spec == nil {
			if err := os.Remove(recordPath(m.dir, m.owner)); err != nil {
				return restore(err)
			}
			return nil
		}
		r.Active, r.Pending = spec, nil
		if err := save(m.dir, r); err != nil {
			return restore(err)
		}
		return nil
	})
}

func checkRangeOwner(requested *Spec, r *record) error {
	if overlap(requested, r.Active) || overlap(requested, r.Pending) {
		return fmt.Errorf("HY2 UDP ports %d-%d on %s conflict with node owner %s", requested.Start, requested.End, requested.Listen, r.Owner)
	}
	return nil
}

// Opening read-only also permits a scalar node with read access to shared
// state to coordinate with range owners without changing any rules or files.
func (m *Manager) withRangeCheck(scalar *Spec, fn func() error) (bool, error) {
	lock, err := os.Open(filepath.Join(m.dir, ".lock"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer lock.Close()
	return true, withFileLock(lock, syscall.LOCK_SH, func() error {
		all, err := records(m.dir)
		if err != nil {
			return err
		}
		for _, r := range all {
			// Even a dead owner can still have installed redirects until cleanup.
			if err := checkRangeOwner(scalar, r); err != nil {
				return err
			}
		}
		return fn()
	})
}

func (m *Manager) activateScalar(scalar *Spec, activate, rollback func() error) error {
	ran, err := m.withRangeCheck(scalar, func() error { return activateOnly(activate, rollback) })
	if ran || err != nil {
		return err
	}
	// The first ever range may create its lock while the scalar is binding.
	// Once the socket exists a later range sees it in /proc; if the range won
	// the race its reservation is now visible, so undo the scalar activation.
	if err := activateOnly(activate, rollback); err != nil {
		return err
	}
	_, err = m.withRangeCheck(scalar, func() error { return nil })
	if err != nil && rollback != nil {
		return errors.Join(err, rollback())
	}
	return err
}

func (m *Manager) Close() error {
	if _, err := os.Stat(recordPath(m.dir, m.owner)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return locked(m.dir, func() error {
		all, err := records(m.dir)
		if err != nil {
			return err
		}
		for _, r := range all {
			if r.Owner == m.owner {
				if alive(r) && r.PID != os.Getpid() {
					return fmt.Errorf("refusing to remove a live node's HY2 rules")
				}
				return cleanup(m.dir, r)
			}
		}
		return nil
	})
}

// A scalar listener has no firewall record. Recognize it by the socket actually
// opened during this manager's activation, including on a rollback restart.
func (m *Manager) TrackListener(listen string, port int, activate func() error) error {
	if port == 0 {
		err := activate()
		if err == nil {
			m.listenerInode = ""
		}
		return err
	}
	if listen == "" {
		listen = "::"
	}
	ip, err := netip.ParseAddr(listen)
	if err != nil {
		return activate()
	}
	base := &Spec{Listen: ip.Unmap().String(), Start: port, End: port}
	before := make(map[string]bool)
	beforeErr := walkSockets(base, func(_ *Spec, inode, state string) error {
		if state == "07" {
			before[inode] = true
		}
		return nil
	})
	if err := activate(); err != nil {
		return err
	}
	old := m.listenerInode
	m.listenerInode = ""
	if beforeErr != nil {
		return nil
	}
	var candidates []string
	err = walkSockets(base, func(_ *Spec, inode, state string) error {
		if state == "07" && (inode == old || !before[inode]) {
			candidates = append(candidates, inode)
		}
		return nil
	})
	if err == nil && len(candidates) == 1 && ownsSocket(candidates[0]) {
		m.listenerInode = candidates[0]
	}
	return nil
}

func ownsSocket(inode string) bool {
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return false
	}
	for _, fd := range fds {
		target, err := os.Readlink("/proc/self/fd/" + fd.Name())
		if err == nil && target == "socket:["+inode+"]" {
			return true
		}
	}
	return false
}

func (m *Manager) checkSockets(spec *Spec) error {
	return walkSockets(spec, func(sock *Spec, inode, state string) error {
		if state == "07" && m.listenerInode != "" && inode == m.listenerInode && ownsSocket(inode) {
			return nil
		}
		return fmt.Errorf("HY2 range includes UDP port %d already in use (%s)", sock.Start, sock.Listen)
	})
}

func walkSockets(spec *Spec, visit func(*Spec, string, string) error) error {
	for _, name := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		data, err := os.ReadFile(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			pair := strings.Split(fields[1], ":")
			if len(pair) != 2 {
				continue
			}
			port, err := strconv.ParseInt(pair[1], 16, 32)
			if err != nil || int(port) < spec.Start || int(port) > spec.End {
				continue
			}
			raw, err := hex.DecodeString(pair[0])
			if err != nil || (len(raw) != 4 && len(raw) != 16) {
				return fmt.Errorf("cannot decode UDP listener address in %s", name)
			}
			for i := 0; i < len(raw); i += 4 {
				word, _ := strconv.ParseUint(pair[0][i*2:i*2+8], 16, 32)
				binary.NativeEndian.PutUint32(raw[i:i+4], uint32(word))
			}
			ip, ok := netip.AddrFromSlice(raw)
			if !ok {
				return fmt.Errorf("invalid UDP listener in %s", name)
			}
			socket := &Spec{Listen: ip.Unmap().String(), Start: int(port), End: int(port)}
			if !overlap(spec, socket) {
				continue
			}
			if err := visit(socket, fields[9], fields[3]); err != nil {
				return err
			}
		}
	}
	return nil
}
