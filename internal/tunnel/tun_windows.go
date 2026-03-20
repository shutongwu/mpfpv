//go:build windows

package tunnel

import "fmt"

func createPlatformTUN(cfg Config) (Device, error) {
	return nil, fmt.Errorf("windows TUN not yet implemented (Phase 4)")
}
