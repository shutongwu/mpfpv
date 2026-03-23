//go:build linux && !android

package transport

// startNetlink attempts to start netlink-based interface monitoring.
// Returns false if unavailable, in which case the caller should fall back
// to polling.
//
// TODO: implement netlink monitoring for instant interface change detection.
// For now we fall back to polling on all platforms.
func (w *InterfaceWatcher) startNetlink() bool {
	return false
}
