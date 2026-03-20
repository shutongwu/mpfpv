//go:build !linux

package transport

func (w *InterfaceWatcher) startNetlink() bool {
	return false // non-Linux platforms do not support netlink
}
