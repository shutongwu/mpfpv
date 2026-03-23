//go:build !linux || android

package transport

func (w *InterfaceWatcher) startNetlink() bool {
	return false // non-Linux platforms do not support netlink
}
