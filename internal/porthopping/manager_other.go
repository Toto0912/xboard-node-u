//go:build !linux

package porthopping

import "fmt"

func (m *Manager) Apply(spec *Spec, activate, rollback func() error) error {
	if spec != nil {
		return fmt.Errorf("automatic HY2 port redirects require Linux; configure manual forwarding and omit port_range")
	}
	return activateOnly(activate, rollback)
}

func (m *Manager) Close() error { return nil }
func CleanupStale() error       { return nil }
func CleanupStopped() error     { return nil }
