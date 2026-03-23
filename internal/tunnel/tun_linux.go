//go:build linux && !android

package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	tunDevice = "/dev/net/tun"
	ifnamsiz  = 16
	iff_TUN   = 0x0001
	iff_NO_PI = 0x1000
	tunSetIff = 0x400454ca // TUNSETIFF ioctl number
)

// ifReq is the ifreq structure used with the TUNSETIFF ioctl.
type ifReq struct {
	Name  [ifnamsiz]byte
	Flags uint16
	_     [22]byte // padding to 40 bytes total
}

// linuxTUN wraps an os.File for the TUN file descriptor.
type linuxTUN struct {
	file *os.File
	name string
}

func createPlatformTUN(cfg Config) (Device, error) {
	// 1. Open /dev/net/tun
	fd, err := syscall.Open(tunDevice, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", tunDevice, err)
	}

	// 2. Configure with ioctl: IFF_TUN | IFF_NO_PI
	var req ifReq
	copy(req.Name[:], cfg.Name)
	req.Flags = iff_TUN | iff_NO_PI

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(tunSetIff), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("ioctl TUNSETIFF: %w", errno)
	}

	// Extract the actual device name assigned by the kernel.
	devName := ""
	for i, b := range req.Name {
		if b == 0 {
			devName = string(req.Name[:i])
			break
		}
	}
	if devName == "" {
		devName = cfg.Name
	}

	// Wrap the fd in an os.File so we get proper Read/Write/Close.
	file := os.NewFile(uintptr(fd), tunDevice)
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("os.NewFile returned nil for fd %d", fd)
	}

	dev := &linuxTUN{
		file: file,
		name: devName,
	}

	// 3. Configure IP address: ip addr add <virtualIP>/<prefix> dev <name>
	if cfg.VirtualIP != nil {
		cidr := fmt.Sprintf("%s/%d", cfg.VirtualIP.String(), cfg.PrefixLen)
		if out, err := exec.Command("ip", "addr", "add", cidr, "dev", devName).CombinedOutput(); err != nil {
			dev.Close()
			return nil, fmt.Errorf("ip addr add %s dev %s: %s: %w", cidr, devName, string(out), err)
		}
	}

	// 4. Bring up the device and set MTU: ip link set <name> up mtu <mtu>
	mtuStr := fmt.Sprintf("%d", cfg.MTU)
	if out, err := exec.Command("ip", "link", "set", devName, "up", "mtu", mtuStr).CombinedOutput(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("ip link set %s up mtu %s: %s: %w", devName, mtuStr, string(out), err)
	}

	return dev, nil
}

func (d *linuxTUN) Read(buf []byte) (int, error) {
	return d.file.Read(buf)
}

func (d *linuxTUN) Write(buf []byte) (int, error) {
	return d.file.Write(buf)
}

func (d *linuxTUN) Close() error {
	return d.file.Close()
}

func (d *linuxTUN) Name() string {
	return d.name
}
