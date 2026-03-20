//go:build darwin

package tunnel

import "fmt"

func createPlatformTUN(cfg Config) (Device, error) {
	return nil, fmt.Errorf("darwin TUN not yet implemented (Phase 4)")
}
