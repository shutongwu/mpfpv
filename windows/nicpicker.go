//go:build windows

package main

import (
	"fmt"
	"net"
)

// NICInfo holds a network interface with its IPv4 address.
type NICInfo struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// ListAllNICs returns all UP network interfaces that have a valid IPv4 address.
// No filtering — shows everything so the user can choose.
func ListAllNICs() ([]NICInfo, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to list interfaces: %w", err)
	}

	var result []NICInfo
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			// Skip link-local.
			if ip4[0] == 169 && ip4[1] == 254 {
				continue
			}

			result = append(result, NICInfo{
				Name: iface.Name,
				IP:   ip4.String(),
			})
			break // one IPv4 per interface
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no network interface with IPv4 found")
	}
	return result, nil
}
