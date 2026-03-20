//go:build windows

package tunnel

import (
	"fmt"
	"net"
	"os/exec"

	log "github.com/sirupsen/logrus"
	wgtun "golang.zx2c4.com/wireguard/tun"
)

// windowsTUN wraps a wireguard/tun Device for Windows (backed by wintun driver).
type windowsTUN struct {
	dev  wgtun.Device
	name string

	// Pre-allocated single-packet batch buffers for Read/Write adaptation.
	readBufs  [][]byte
	readSizes []int
}

func createPlatformTUN(cfg Config) (Device, error) {
	// 1. Create the TUN device via wintun driver.
	dev, err := wgtun.CreateTUN(cfg.Name, cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("create wintun device: %w", err)
	}

	// 2. Get the actual device name assigned by the system.
	name, err := dev.Name()
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("get device name: %w", err)
	}
	log.Infof("TUN device created: %s", name)

	// 3. Configure IP address using netsh.
	if cfg.VirtualIP != nil {
		mask := cidrToMask(cfg.PrefixLen)
		cmd := exec.Command("netsh", "interface", "ip", "set", "address",
			fmt.Sprintf("name=%s", name), "static", cfg.VirtualIP.String(), mask)
		if out, err := cmd.CombinedOutput(); err != nil {
			dev.Close()
			return nil, fmt.Errorf("netsh set address %s %s/%d: %s: %w",
				name, cfg.VirtualIP, cfg.PrefixLen, string(out), err)
		}
		log.Infof("TUN address configured: %s/%d on %s", cfg.VirtualIP, cfg.PrefixLen, name)
	}

	// Pre-allocate batch buffers (batch size = 1 for our simple API).
	readBufs := make([][]byte, 1)
	readBufs[0] = make([]byte, cfg.MTU+200) // extra room for safety
	readSizes := make([]int, 1)

	return &windowsTUN{
		dev:       dev,
		name:      name,
		readBufs:  readBufs,
		readSizes: readSizes,
	}, nil
}

func (t *windowsTUN) Read(buf []byte) (int, error) {
	// wireguard/tun uses batch API: Read(bufs [][]byte, sizes []int, offset int).
	// We read into our pre-allocated buffer, then copy to the caller's buf.
	for {
		n, err := t.dev.Read(t.readBufs, t.readSizes, 0)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		// Copy the first (and only) packet into the caller's buffer.
		size := t.readSizes[0]
		if size > len(buf) {
			size = len(buf)
		}
		copy(buf[:size], t.readBufs[0][:size])
		return size, nil
	}
}

func (t *windowsTUN) Write(buf []byte) (int, error) {
	// wireguard/tun uses batch API: Write(bufs [][]byte, offset int).
	bufs := [][]byte{buf}
	_, err := t.dev.Write(bufs, 0)
	if err != nil {
		return 0, err
	}
	return len(buf), nil
}

func (t *windowsTUN) Close() error {
	return t.dev.Close()
}

func (t *windowsTUN) Name() string {
	return t.name
}

// cidrToMask converts a prefix length to a dotted-decimal subnet mask string.
func cidrToMask(prefixLen int) string {
	mask := net.CIDRMask(prefixLen, 32)
	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
}
