// Package porthopping owns the host-side UDP redirects, never QUIC connections.
package porthopping

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/cedar2025/xboard-node/internal/config"
)

type Spec struct {
	Listen string
	Start  int
	End    int
}

func NewSpec(listen, portRange string, port int) (*Spec, error) {
	if portRange == "" {
		return nil, nil
	}
	start, end, err := config.ParseHY2PortRange(portRange)
	if err != nil {
		return nil, err
	}
	if start != port {
		return nil, fmt.Errorf("HY2 port range must start with server_port")
	}
	if listen == "" {
		listen = "::"
	}
	ip, err := netip.ParseAddr(listen)
	if err != nil || ip.Zone() != "" {
		return nil, fmt.Errorf("HY2 automatic redirects require a literal listening IP")
	}
	return &Spec{Listen: ip.Unmap().String(), Start: start, End: end}, nil
}

type Manager struct {
	owner         string
	dir           string
	listenerInode string
}

func stateDirectory() string {
	if value := os.Getenv("XBOARD_NODE_PORT_HOPPING_STATE_DIR"); value != "" {
		return value
	}
	return "/run/xboard-node/port-hopping"
}

func New(identity string) *Manager {
	sum := sha256.Sum256([]byte(filepath.Clean(identity)))
	return &Manager{owner: fmt.Sprintf("%x", sum[:10]), dir: stateDirectory()}
}

func same(a, b *Spec) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func activateOnly(activate, rollback func() error) error {
	err := activate()
	if err != nil && rollback != nil {
		return errors.Join(err, rollback())
	}
	return err
}

func overlap(a, b *Spec) bool {
	if a == nil || b == nil || a.End < b.Start || b.End < a.Start {
		return false
	}
	return a.Listen == "::" || b.Listen == "::" || a.Listen == b.Listen ||
		(a.Listen == "0.0.0.0" && netip.MustParseAddr(b.Listen).Is4()) ||
		(b.Listen == "0.0.0.0" && netip.MustParseAddr(a.Listen).Is4())
}
