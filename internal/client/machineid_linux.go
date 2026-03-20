//go:build linux

package client

import (
	"os"
	"strings"
)

// machineID returns a stable machine identifier.
// On Linux, reads /etc/machine-id (systemd) or /var/lib/dbus/machine-id.
func machineID() string {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		data, err := os.ReadFile(path)
		if err == nil {
			id := strings.TrimSpace(string(data))
			if id != "" {
				return id
			}
		}
	}
	return "linux-unknown"
}
