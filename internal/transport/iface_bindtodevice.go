//go:build linux

package transport

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// createBoundUDPConn creates a UDP connection bound to a specific network
// interface using SO_BINDTODEVICE on Linux. The socket is also bound to the
// given local address.
func createBoundUDPConn(localAddr net.IP, ifaceName string) (*net.UDPConn, error) {
	laddr := &net.UDPAddr{IP: localAddr, Port: 0}

	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("socket create (iface=%s): %w", ifaceName, err)
	}

	if err := syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("SO_REUSEADDR (iface=%s): %w", ifaceName, err)
	}

	if ifaceName != "" {
		if err := syscall.SetsockoptString(s, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifaceName); err != nil {
			syscall.Close(s)
			return nil, fmt.Errorf("SO_BINDTODEVICE (iface=%s): %w", ifaceName, err)
		}
	}

	lsa := syscall.SockaddrInet4{Port: laddr.Port}
	copy(lsa.Addr[:], laddr.IP.To4())

	if err := syscall.Bind(s, &lsa); err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("bind (iface=%s, addr=%v): %w", ifaceName, laddr, err)
	}

	f := os.NewFile(uintptr(s), "")
	c, err := net.FilePacketConn(f)
	f.Close()
	if err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("FilePacketConn (iface=%s): %w", ifaceName, err)
	}

	return c.(*net.UDPConn), nil
}
