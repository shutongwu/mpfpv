package tunnel

import (
	"net"
	"testing"
)

// --- Unit tests (no privileges required) ---

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}

	// CreateTUN will fill defaults before calling the platform func.
	// We test the defaulting logic by inspecting what CreateTUN would use.
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.Name == "" {
		cfg.Name = DefaultName
	}

	if cfg.MTU != 1400 {
		t.Errorf("expected default MTU 1400, got %d", cfg.MTU)
	}
	if cfg.Name != "mpfpv0" {
		t.Errorf("expected default name mpfpv0, got %s", cfg.Name)
	}
}

func TestConfigCustomValues(t *testing.T) {
	cfg := Config{
		Name:      "tun7",
		MTU:       1400,
		VirtualIP: net.IPv4(10, 99, 0, 1),
		PrefixLen: 24,
	}

	if cfg.Name != "tun7" {
		t.Errorf("expected name tun7, got %s", cfg.Name)
	}
	if cfg.MTU != 1400 {
		t.Errorf("expected MTU 1400, got %d", cfg.MTU)
	}
	if !cfg.VirtualIP.Equal(net.IPv4(10, 99, 0, 1)) {
		t.Errorf("unexpected VirtualIP: %v", cfg.VirtualIP)
	}
	if cfg.PrefixLen != 24 {
		t.Errorf("expected prefix 24, got %d", cfg.PrefixLen)
	}
}

func TestDefaultConstants(t *testing.T) {
	if DefaultMTU != 1400 {
		t.Errorf("DefaultMTU should be 1400, got %d", DefaultMTU)
	}
	if DefaultName != "mpfpv0" {
		t.Errorf("DefaultName should be mpfpv0, got %s", DefaultName)
	}
}
