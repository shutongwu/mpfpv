package transport

import (
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// InterfaceInfo describes a network interface and its IPv4 addresses.
type InterfaceInfo struct {
	Name  string
	Addrs []net.IP // IPv4 addresses on this interface
	IsUp  bool
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
			if !sameAddrs(oldInfo.Addrs, newInfo.Addrs) {
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

// isVirtualInterface returns true if the interface name matches common
// virtual/tunnel device patterns that should never be used as send paths.
func isVirtualInterface(name string) bool {
	prefixes := []string{
		"mpfpv",  // our own TUN device — MUST exclude to prevent packet loops
		"tun",    // generic TUN devices
		"tap",    // TAP devices
		"veth",   // virtual ethernet pairs (Docker, LXC, netns)
		"br-",    // Docker bridge networks
		"vmbr",   // Proxmox VE bridges
		"virbr",  // libvirt bridges
		"cni",    // Kubernetes CNI
		"flannel", // Kubernetes flannel
		"calico", // Kubernetes calico
		"wg",     // WireGuard interfaces
	}
	for _, p := range prefixes {
		if len(name) >= len(p) && name[:len(p)] == p {
			return true
		}
	}
	return false
}

// scanInterfaces returns the set of usable network interfaces with IPv4
// addresses, excluding loopback, down, excluded, virtual devices, and
// those without IPv4.
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
		// Skip known virtual/tunnel interfaces to prevent packet loops.
		if isVirtualInterface(name) {
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
			// Skip link-local (169.254.x.x).
			if ip4[0] == 169 && ip4[1] == 254 {
				continue
			}
			ipv4s = append(ipv4s, ip4)
		}

		if len(ipv4s) == 0 {
			continue
		}

		result[name] = &InterfaceInfo{
			Name:  name,
			Addrs: ipv4s,
			IsUp:  true,
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
