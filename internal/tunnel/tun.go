package tunnel

import "net"

// Device is the interface for a TUN device.
type Device interface {
	// Read reads a single IP packet into buf. n is the number of bytes read.
	Read(buf []byte) (n int, err error)
	// Write writes a single IP packet.
	Write(buf []byte) (n int, err error)
	// Close closes the TUN device.
	Close() error
	// Name returns the device name (e.g. "tun0", "utun3").
	Name() string
}

// Config holds the configuration for creating a TUN device.
type Config struct {
	Name      string // Desired device name (Linux honours this; other platforms may ignore it).
	MTU       int    // MTU, fixed 1400.
	VirtualIP net.IP // Virtual IP address to assign.
	PrefixLen int    // Subnet prefix length, e.g. 24.
}

// DefaultMTU is the fixed MTU for all mpfpv TUN devices.
const DefaultMTU = 1400

// DefaultName is the default TUN device name.
const DefaultName = "mpfpv0"

// CreateTUN creates and configures a TUN device using the platform-specific
// implementation.
func CreateTUN(cfg Config) (Device, error) {
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.Name == "" {
		cfg.Name = DefaultName
	}
	return createPlatformTUN(cfg)
}
