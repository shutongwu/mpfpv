//go:build !android

package tunnel

// SetTunRequestCallback is a no-op on non-Android platforms.
func SetTunRequestCallback(cb func(ip string, prefixLen int, mtu int)) {}

// SetTunFD is a no-op on non-Android platforms.
func SetTunFD(fd int) {}
