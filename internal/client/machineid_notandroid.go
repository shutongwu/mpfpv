//go:build !android

package client

// SetAndroidMachineID is a no-op on non-Android platforms.
func SetAndroidMachineID(id string) {}
