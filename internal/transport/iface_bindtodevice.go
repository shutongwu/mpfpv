//go:build linux && !android

package transport

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// createBoundUDPConn creates a UDP connection bound to a specific network
// interface using SO_BINDTODEVICE on Linux. The socket is also bound to the
// given local address. Supports both IPv4 and IPv6 addresses.
func createBoundUDPConn(localAddr net.IP, ifaceName string) (*net.UDPConn, error) {
	laddr := &net.UDPAddr{IP: localAddr, Port: 0}

	// Determine address family from localAddr.
	isIPv6 := localAddr.To4() == nil
	af := syscall.AF_INET
	if isIPv6 {
		af = syscall.AF_INET6
	}

	s, err := syscall.Socket(af, syscall.SOCK_DGRAM, syscall.IPPROTO_UDP)
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

	// Set a kernel-level send timeout so WriteToUDP does not block
	// when the NIC is physically unplugged. Without this, serial
	// redundant sends stall the healthy NIC for 2-3 seconds.
	tv := syscall.Timeval{Sec: 0, Usec: 50000} // 50ms
	if err := syscall.SetsockoptTimeval(s, syscall.SOL_SOCKET, syscall.SO_SNDTIMEO, &tv); err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("SO_SNDTIMEO (iface=%s): %w", ifaceName, err)
	}

	// Bind to the appropriate sockaddr type.
	var sa syscall.Sockaddr
	if isIPv6 {
		lsa := syscall.SockaddrInet6{Port: laddr.Port}
		copy(lsa.Addr[:], localAddr.To16())
		sa = &lsa
	} else {
		lsa := syscall.SockaddrInet4{Port: laddr.Port}
		copy(lsa.Addr[:], localAddr.To4())
		sa = &lsa
	}

	if err := syscall.Bind(s, sa); err != nil {
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
