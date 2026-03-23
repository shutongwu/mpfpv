package client

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/cloud/mpfpv/internal/protocol"
	"github.com/cloud/mpfpv/internal/web"
)

// GetStatus returns the client's current connection status.
func (c *Client) GetStatus() web.StatusInfo {
	vipIP := c.virtualIPVal.Load().(net.IP)
	vip := ""
	if vipIP != nil {
		vip = vipIP.String()
	}
	c.mu.Lock()
	sm := c.sendMode
	c.mu.Unlock()

	modeStr := "redundant"
	if sm == protocol.SendModeFailover {
		modeStr = "failover"
	}

	return web.StatusInfo{
		Connected: atomic.LoadInt32(&c.registered) == 1,
		VirtualIP: vip,
		ClientID:  c.cfg.Client.ClientID,
		SendMode:  modeStr,
	}
}

// GetInterfaces returns the status of all network interfaces used by the client.
func (c *Client) GetInterfaces() []web.InterfaceStatus {
	if c.multipath == nil {
		return []web.InterfaceStatus{}
	}

	paths := c.multipath.GetPaths()
	result := make([]web.InterfaceStatus, 0, len(paths))
	for _, p := range paths {
		rttStr := "-"
		if p.RTT > 0 {
			rttStr = p.RTT.Round(time.Millisecond).String()
		}
		result = append(result, web.InterfaceStatus{
			Name:     p.IfaceName,
			LocalIP:  p.LocalAddr,
			Status:   p.Status,
			RTT:      rttStr,
			IsActive: p.IsActive,
		})
	}
	return result
}

// SetInterfaceEnabled enables or disables a network interface.
// Phase 4 simple implementation: this is a placeholder that logs the intent
// but does not actually close the socket. Full implementation would remove
// the path from the multipath sender.
func (c *Client) SetInterfaceEnabled(name string, enabled bool) error {
	if c.multipath == nil {
		return fmt.Errorf("multipath not active")
	}
	// Phase 4 placeholder: just validate the interface exists.
	paths := c.multipath.GetPaths()
	found := false
	for _, p := range paths {
		if p.IfaceName == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("interface %q not found", name)
	}
	// TODO: actually disable/enable the path socket in a future phase.
	return nil
}

// SetSendMode switches the client's send mode at runtime.
func (c *Client) SetSendMode(mode string) error {
	var sm uint8
	switch mode {
	case "redundant":
		sm = protocol.SendModeRedundant
	case "failover":
		sm = protocol.SendModeFailover
	default:
		return fmt.Errorf("invalid sendMode %q, must be 'redundant' or 'failover'", mode)
	}

	c.mu.Lock()
	c.sendMode = sm
	c.mu.Unlock()

	// Also update the multipath sender's mode if available.
	if c.multipath != nil {
		c.multipath.SetSendMode(sm)
	}

	return nil
}
