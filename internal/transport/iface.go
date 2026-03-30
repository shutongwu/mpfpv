package transport

import (
	"net"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// InterfaceInfo describes a network interface and its addresses.
type InterfaceInfo struct {
	Name   string
	Addrs  []net.IP // IPv4 addresses on this interface
	Addrs6 []net.IP // IPv6 global/ULA addresses on this interface
	IsUp   bool
}

// InterfaceWatcher monitors network interface changes and invokes a callback
// when interfaces are added or removed.
type InterfaceWatcher struct {
	excluded map[string]bool
	current  map[string]*InterfaceInfo
	onChange func(added, removed []InterfaceInfo)
	mu       sync.Mutex
	stopCh   chan struct{}
}

// NewInterfaceWatcher creates a watcher that excludes the given interface names
// and calls onChange when the set of usable interfaces changes.
func NewInterfaceWatcher(excluded []string, onChange func(added, removed []InterfaceInfo)) *InterfaceWatcher {
	ex := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		ex[name] = true
	}
	return &InterfaceWatcher{
		excluded: ex,
		current:  make(map[string]*InterfaceInfo),
		onChange: onChange,
		stopCh:   make(chan struct{}),
	}
}

// Start begins monitoring. On Linux it tries netlink first; if that fails it
// falls back to polling.
func (w *InterfaceWatcher) Start() error {
	// Perform an initial scan so callers can see current interfaces immediately.
	w.mu.Lock()
	w.current = w.scanInterfaces()
	w.mu.Unlock()

	if w.startNetlink() {
		log.Info("iface watcher: using netlink")
		return nil
	}
	log.Info("iface watcher: using polling (200ms)")
	go w.pollInterfaces(200 * time.Millisecond)
	return nil
}

// Stop stops the watcher.
func (w *InterfaceWatcher) Stop() {
	select {
	case <-w.stopCh:
		// already stopped
	default:
		close(w.stopCh)
	}
}

// Current returns a snapshot of the currently known interfaces.
func (w *InterfaceWatcher) Current() map[string]*InterfaceInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]*InterfaceInfo, len(w.current))
	for k, v := range w.current {
		out[k] = v
	}
	return out
}

// pollInterfaces is the fallback polling implementation. It scans interfaces
// every interval and fires onChange when the set changes.
func (w *InterfaceWatcher) pollInterfaces(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.detectChanges()
		}
	}
}

// detectChanges scans interfaces and fires the onChange callback if the set
// has changed. It is safe for concurrent use.
func (w *InterfaceWatcher) detectChanges() {
	newSet := w.scanInterfaces()

	w.mu.Lock()
	defer w.mu.Unlock()

	var added, removed []InterfaceInfo

	// Detect new interfaces.
	for name, info := range newSet {
		if _, exists := w.current[name]; !exists {
			added = append(added, *info)
		}
	}
	// Detect removed interfaces.
	for name, info := range w.current {
		if _, exists := newSet[name]; !exists {
			removed = append(removed, *info)
		}
	}
	// Detect address changes (treat as remove + add).
	for name, newInfo := range newSet {
		if oldInfo, exists := w.current[name]; exists {
			if !sameAddrs(oldInfo.Addrs, newInfo.Addrs) || !sameAddrs(oldInfo.Addrs6, newInfo.Addrs6) {
				removed = append(removed, *oldInfo)
				added = append(added, *newInfo)
			}
		}
	}

	w.current = newSet

	if len(added) > 0 || len(removed) > 0 {
		if w.onChange != nil {
			w.onChange(added, removed)
		}
	}
}

// isPhysicalInterface returns true if the interface name looks like a real
// physical NIC (WiFi, Ethernet, USB adapter). Everything else is ignored.
func isPhysicalInterface(name string) bool {
	// Linux physical NIC patterns
	linuxPrefixes := []string{
		"eth",   // traditional ethernet
		"enp",   // systemd predictable: PCI ethernet
		"ens",   // systemd predictable: hotplug slot
		"eno",   // systemd predictable: onboard
		"enx",   // systemd predictable: MAC-based (USB dongles)
		"wlan",  // traditional WiFi
		"wlp",   // systemd predictable: PCI WiFi
		"wlx",   // systemd predictable: USB WiFi
		"usb",   // USB network adapters
	}
	for _, p := range linuxPrefixes {
		if len(name) >= len(p) && name[:len(p)] == p {
			return true
		}
	}

	// Windows: names contain these keywords (localized names vary)
	winKeywords := []string{
		"Wi-Fi", "WiFi", "Wireless", "WLAN",
		"Ethernet", "以太网",
		"USB",
	}
	for _, kw := range winKeywords {
		if strings.Contains(name, kw) {
			return true
		}
	}

	// macOS
	if len(name) >= 2 && name[:2] == "en" {
		return true
	}

	return false
}

// scanInterfaces returns the set of usable network interfaces with IPv4
// and/or IPv6 addresses, excluding loopback, down, excluded, and virtual
// devices.
func (w *InterfaceWatcher) scanInterfaces() map[string]*InterfaceInfo {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.WithError(err).Warn("scanInterfaces: net.Interfaces failed")
		return nil
	}

	result := make(map[string]*InterfaceInfo)
	for _, iface := range ifaces {
		name := iface.Name

		// Skip excluded.
		if w.excluded[name] {
			continue
		}
		// Only use real physical NICs (WiFi, Ethernet, USB adapters).
		if !isPhysicalInterface(name) {
			continue
		}
		// Skip loopback.
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		// Skip down.
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		var ipv4s []net.IP
		var ipv6s []net.IP
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

			// IPv4 path.
			if ip4 := ip.To4(); ip4 != nil {
				// Skip link-local (169.254.x.x).
				if ip4[0] == 169 && ip4[1] == 254 {
					continue
				}
				// Skip CGNAT/Tailscale (100.64.0.0/10).
				if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
					continue
				}
				// Skip our own virtual IP range (10.99.x.x).
				if ip4[0] == 10 && ip4[1] == 99 {
					continue
				}
				ipv4s = append(ipv4s, ip4)
				continue
			}

			// IPv6 path.
			ip6 := ip.To16()
			if ip6 == nil {
				continue
			}
			// Skip link-local (fe80::/10).
			if ip6[0] == 0xfe && ip6[1]&0xc0 == 0x80 {
				continue
			}
			// Skip loopback (::1).
			if ip6.Equal(net.IPv6loopback) {
				continue
			}
			// Accept global unicast (2000::/3) and ULA (fc00::/7).
			if (ip6[0]&0xe0 == 0x20) || (ip6[0]&0xfe == 0xfc) {
				ipv6s = append(ipv6s, ip6)
			}
		}

		if len(ipv4s) == 0 && len(ipv6s) == 0 {
			continue
		}

		result[name] = &InterfaceInfo{
			Name:   name,
			Addrs:  ipv4s,
			Addrs6: ipv6s,
			IsUp:   true,
		}
	}
	return result
}

// sameAddrs returns true if two IP slices contain the same addresses
// (order-insensitive).
func sameAddrs(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, ip := range a {
		set[ip.String()] = true
	}
	for _, ip := range b {
		if !set[ip.String()] {
			return false
		}
	}
	return true
}
