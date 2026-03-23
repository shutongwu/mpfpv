//go:build android

package tunnel

import (
	"fmt"
	"os"
	"sync"
)

// androidTunProvider holds the callback and fd channel for Android VpnService integration.
var androidTun struct {
	mu        sync.Mutex
	requestCB func(ip string, prefixLen int, mtu int) // called when TUN is needed
	fdCh      chan int                                  // receives fd from Java
}

func init() {
	androidTun.fdCh = make(chan int, 1)
}

// SetTunRequestCallback registers the callback invoked when the client needs
// a TUN device. The Java layer should call VpnService.Builder with the given
// IP/prefix/MTU, call establish(), and then call SetTunFD with the resulting fd.
func SetTunRequestCallback(cb func(ip string, prefixLen int, mtu int)) {
	androidTun.mu.Lock()
	androidTun.requestCB = cb
	androidTun.mu.Unlock()
}

// SetTunFD provides the TUN file descriptor obtained from VpnService.establish().
func SetTunFD(fd int) {
	androidTun.fdCh <- fd
}

type androidTUNDevice struct {
	file *os.File
}

func createPlatformTUN(cfg Config) (Device, error) {
	androidTun.mu.Lock()
	cb := androidTun.requestCB
	androidTun.mu.Unlock()

	if cb == nil {
		return nil, fmt.Errorf("android: TUN request callback not set")
	}

	ipStr := ""
	if cfg.VirtualIP != nil {
		ipStr = cfg.VirtualIP.String()
	}

	// Ask Java to create VpnService with this IP.
	cb(ipStr, cfg.PrefixLen, cfg.MTU)

	// Wait for Java to provide the fd.
	fd := <-androidTun.fdCh
	if fd < 0 {
		return nil, fmt.Errorf("android: VpnService returned invalid fd %d", fd)
	}

	file := os.NewFile(uintptr(fd), "vpn-tun")
	if file == nil {
		return nil, fmt.Errorf("android: os.NewFile returned nil for fd %d", fd)
	}

	return &androidTUNDevice{file: file}, nil
}

func (d *androidTUNDevice) Read(buf []byte) (int, error)  { return d.file.Read(buf) }
func (d *androidTUNDevice) Write(buf []byte) (int, error) { return d.file.Write(buf) }
func (d *androidTUNDevice) Close() error                   { return d.file.Close() }
func (d *androidTUNDevice) Name() string                   { return "tun-android" }
