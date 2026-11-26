package config

import (
	"path/filepath"
	"testing"
)

func TestStaticLegacyCertDirectory(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent-certs")
	nodeKernel := filepath.Join(base, "override")
	cfg := Config{Kernel: KernelConfig{ConfigDir: base}, Cert: CertConfig{CertDir: parent}}
	tests := []struct {
		name   string
		entry  NodeEntry
		legacy string
	}{
		{"inherited custom", NodeEntry{NodeID: 1}, parent},
		{"kernel section inherited parent", NodeEntry{NodeID: 1, Kernel: &KernelOverride{}, Cert: &CertConfig{}}, filepath.Join(base, "certs")},
		{"previously isolated", NodeEntry{NodeID: 1, Cert: &CertConfig{}}, ""},
		{"kernel directory explicit", NodeEntry{NodeID: 1, Kernel: &KernelOverride{ConfigDir: nodeKernel}, Cert: &CertConfig{}}, ""},
		{"certificate directory explicit", NodeEntry{NodeID: 1, Cert: &CertConfig{CertDir: nodeKernel}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg.Nodes = []NodeEntry{tc.entry}
			if got := cfg.ExpandNodes()[0].Cert.LegacyCertDir; got != tc.legacy {
				t.Fatalf("old directory = %q, want %q", got, tc.legacy)
			}
		})
	}
}

func TestStartupCertDirectoryUsesActualMachineLayout(t *testing.T) {
	base := t.TempDir()
	machine := &Config{InstanceID: "machine", Machine: &MachineConfig{MachineID: 1}, Kernel: KernelConfig{ConfigDir: base}}
	node := &Config{InstanceID: "static", Panel: PanelConfig{NodeID: 2}, Kernel: KernelConfig{ConfigDir: filepath.Join(base, "static")}}
	node.Cert.CertDir = machine.ExpandMachineNode(42, "").Cert.CertDir
	for _, order := range [][]*Config{{node, machine}, {machine, node}} {
		if err := ValidateStartupLayout(order); err == nil {
			t.Fatal("static and discovered node share a cert_dir")
		}
	}
	machine.Cert.CertDir = filepath.Join(base, "unused-base")
	node.Cert.CertDir = machine.Cert.CertDir
	if err := ValidateStartupLayout([]*Config{machine, node}); err != nil {
		t.Fatalf("unused machine base rejected: %v", err)
	}
	second := &Config{InstanceID: "other", Panel: PanelConfig{NodeID: 3}, Cert: CertConfig{CertDir: node.Cert.CertDir}}
	if err := ValidateStartupLayout([]*Config{node, second}); err == nil {
		t.Fatal("cross-instance static cert_dir collision accepted")
	}
}
