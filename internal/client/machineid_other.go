//go:build !linux && !windows

package client

import "os"

// machineID returns a stable machine identifier.
// Fallback for unsupported platforms: use hostname.
func machineID() string {
	name, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return "other-" + name
}
