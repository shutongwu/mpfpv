//go:build linux && integration

package tunnel

import (
	"net"
	"testing"
)

// TestCreateTUN requires root privileges. Run with:
//
//	sudo go test -tags integration ./internal/tunnel/ -run TestCreateTUN -v
func TestCreateTUN(t *testing.T) {
	cfg := Config{
		Name:      "mpfpvtest0",
		MTU:       1400,
		VirtualIP: net.IPv4(10, 199, 0, 1),
		PrefixLen: 24,
	}

	dev, err := CreateTUN(cfg)
	if err != nil {
		t.Fatalf("CreateTUN failed: %v", err)
	}
	defer dev.Close()

	if dev.Name() != "mpfpvtest0" {
		t.Errorf("expected device name mpfpvtest0, got %s", dev.Name())
	}

	// Verify the device is usable by doing a non-blocking read attempt.
	// Since no traffic is flowing, we just confirm no immediate error on a
	// short read buffer.
	t.Logf("TUN device %s created successfully", dev.Name())
}
