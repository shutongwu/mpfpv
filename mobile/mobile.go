// Package mobile provides gomobile-bindable API for the mpfpv Android client.
package mobile

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"time"

	"github.com/cloud/mpfpv/internal/client"
	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/tunnel"
	"github.com/cloud/mpfpv/internal/web"
	log "github.com/sirupsen/logrus"
)

// TunCallback is implemented by the Android Java/Kotlin layer.
type TunCallback interface {
	// OnProtectSocket is called with the UDP socket fd before VPN is established.
	// Java must call VpnService.protect(fd) to prevent routing loop.
	OnProtectSocket(fd int)

	// OnTunRequest is called when the Go client needs a TUN device.
	// Java should create VpnService.Builder with the given IP/prefix/MTU,
	// call establish(), and then call SetTunFD() with the resulting fd.
	OnTunRequest(ip string, prefixLen int, mtu int)

	// OnStatusChange is called when connection status changes.
	OnStatusChange(connected bool, virtualIP string)
}

var (
	mu       sync.Mutex
	cli      *client.Client
	cancel   context.CancelFunc
	running  bool
	callback TunCallback
	webPort  int
)

// Start starts the mpfpv client.
func Start(serverAddr, teamKey, deviceName, machineID, webUIAddr string, cb TunCallback) error {
	mu.Lock()
	if running {
		mu.Unlock()
		return nil
	}
	mu.Unlock()

	// Set Android machine ID before client creation.
	client.SetAndroidMachineID(machineID)

	// Register TUN request callback: bridge from tunnel package to Java callback.
	// Before creating the VPN, protect the UDP socket to prevent routing loop.
	tunnel.SetTunRequestCallback(func(ip string, prefixLen int, mtu int) {
		mu.Lock()
		c := cli
		mu.Unlock()
		if c != nil {
			fd := c.SocketFD()
			if fd >= 0 {
				cb.OnProtectSocket(fd)
				log.Infof("mobile: protected socket fd=%d", fd)
			}
		}
		cb.OnTunRequest(ip, prefixLen, mtu)
	})

	// Find the first usable network interface for single-socket mode.
	// On Android, multipath doesn't work (interface names like rmnet0 aren't
	// recognized, CGNAT IPs get filtered). Use BindInterface to force
	// single-socket mode and skip multipath entirely.
	// If no interface found, still set a dummy to trigger single-socket path
	// but client.Run will fail on bind, so we handle that by not setting it.
	bindIface := findActiveInterface()
	log.Infof("mobile: active interface=%q", bindIface)

	// Build config programmatically.
	cfg := &config.Config{
		Mode:    "client",
		TeamKey: teamKey,
		Client: &config.ClientConfig{
			ServerAddr:    serverAddr,
			SendMode:      "redundant",
			MTU:           1400,
			DedupWindow:   4096,
			WebUI:         webUIAddr,
			BindInterface: bindIface,
		},
	}
	if deviceName != "" {
		cfg.Client.DeviceName = deviceName
	}

	c, err := client.New(cfg)
	if err != nil {
		return err
	}

	ctx, cancelFunc := context.WithCancel(context.Background())

	mu.Lock()
	cli = c
	cancel = cancelFunc
	running = true
	callback = cb
	mu.Unlock()

	// Parse web port from webUIAddr.
	if webUIAddr != "" {
		// Start Web UI.
		handler := web.NewHandler("client", c)
		go func() {
			if err := web.StartWebUI(webUIAddr, handler); err != nil {
				log.WithError(err).Error("mobile: web UI error")
			}
		}()
		// Extract port number.
		for i := len(webUIAddr) - 1; i >= 0; i-- {
			if webUIAddr[i] == ':' {
				p := 0
				for _, ch := range webUIAddr[i+1:] {
					p = p*10 + int(ch-'0')
				}
				mu.Lock()
				webPort = p
				mu.Unlock()
				break
			}
		}
	}

	// Run client in background.
	go func() {
		if err := c.Run(ctx); err != nil {
			log.WithError(err).Error("mobile: client error")
		}
		mu.Lock()
		running = false
		cli = nil
		mu.Unlock()
	}()

	// Status polling goroutine.
	go func() {
		lastConnected := false
		lastIP := ""
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}

			mu.Lock()
			c := cli
			cb := callback
			mu.Unlock()

			if c == nil || cb == nil {
				continue
			}

			status := c.GetStatus()
			if status.Connected != lastConnected || status.VirtualIP != lastIP {
				lastConnected = status.Connected
				lastIP = status.VirtualIP
				cb.OnStatusChange(status.Connected, status.VirtualIP)
			}
		}
	}()

	return nil
}

// SetTunFD provides the TUN file descriptor from Android VpnService.
// Call this from Java after VpnService.establish() succeeds.
func SetTunFD(fd int) {
	tunnel.SetTunFD(fd)
}

// Stop stops the mpfpv client.
func Stop() {
	mu.Lock()
	if cancel != nil {
		cancel()
		cancel = nil
	}
	running = false
	callback = nil
	mu.Unlock()
}

// IsConnected returns whether the client is registered with the server.
func IsConnected() bool {
	mu.Lock()
	c := cli
	mu.Unlock()
	if c == nil {
		return false
	}
	return c.IsRegistered()
}

// GetVirtualIP returns the assigned virtual IP, or empty string if not connected.
func GetVirtualIP() string {
	mu.Lock()
	c := cli
	mu.Unlock()
	if c == nil {
		return ""
	}
	status := c.GetStatus()
	return status.VirtualIP
}

// GetWebPort returns the Web UI port number.
func GetWebPort() int {
	mu.Lock()
	defer mu.Unlock()
	return webPort
}

// GetStatusJSON returns the client status as JSON string.
func GetStatusJSON() string {
	mu.Lock()
	c := cli
	mu.Unlock()
	if c == nil {
		return "{}"
	}
	status := c.GetStatus()
	data, _ := json.Marshal(status)
	return string(data)
}

// findActiveInterface returns the name of the first usable network interface
// with an IPv4 address. This is used to force single-socket mode on Android.
func findActiveInterface() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			// Skip link-local and our VPN range only.
			// Don't skip CGNAT — Android 4G/5G uses CGNAT addresses.
			if ip4[0] == 169 && ip4[1] == 254 {
				continue
			}
			if ip4[0] == 10 && ip4[1] == 99 {
				continue
			}
			return iface.Name
		}
	}
	return ""
}
