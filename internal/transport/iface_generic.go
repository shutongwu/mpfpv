//go:build !linux

package transport

import (
	"net"
)

// createBoundUDPConn creates a UDP connection bound to the given local address.
// On non-Linux platforms SO_BINDTODEVICE is not available, so we rely on
// address binding to route traffic through the correct interface.
func createBoundUDPConn(localAddr net.IP, ifaceName string) (*net.UDPConn, error) {
	laddr := &net.UDPAddr{IP: localAddr, Port: 0}
	return net.ListenUDP("udp4", laddr)
}
