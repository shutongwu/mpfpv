//go:build windows

package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cloud/mpfpv/internal/client"
	"github.com/cloud/mpfpv/internal/config"
	log "github.com/sirupsen/logrus"
)

// Controller manages the client lifecycle for the simplified GUI.
type Controller struct {
	cfgPath    string
	cfg        *config.Config
	client     *client.Client
	cancelFunc context.CancelFunc
	mu         sync.Mutex
	connected  bool
	lastError  string
	lastReq    *ConnectRequest // saved for auto-reconnect
	boundIP    string          // IP when connection was established
	stopWatch  context.CancelFunc
}

// NewController creates a new GUI controller.
func NewController(cfgPath string, cfg *config.Config) *Controller {
	return &Controller{
		cfgPath: cfgPath,
		cfg:     cfg,
	}
}

// SimpleStatus is returned by the status API.
type SimpleStatus struct {
	Connected bool   `json:"connected"`
	VirtualIP string `json:"virtualIP"`
	NICName   string `json:"nicName"`
	Error     string `json:"error"`
}

// ConfigInfo is returned/accepted by the config API.
type ConfigInfo struct {
	ServerAddr    string `json:"serverAddr"`
	TeamKey       string `json:"teamKey"`
	BindInterface string `json:"bindInterface"`
}

// ConnectRequest is the request body for /api/connect.
type ConnectRequest struct {
	ServerAddr    string `json:"serverAddr"`
	TeamKey       string `json:"teamKey"`
	BindInterface string `json:"bindInterface"`
}

// GetStatus returns the current connection status.
func (c *Controller) GetStatus() SimpleStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := SimpleStatus{
		Connected: c.connected,
		Error:     c.lastError,
	}

	if c.cfg != nil && c.cfg.Client != nil && c.cfg.Client.BindInterface != "" {
		s.NICName = c.cfg.Client.BindInterface
	}

	if c.client != nil {
		st := c.client.GetStatus()
		s.VirtualIP = st.VirtualIP
		s.Connected = st.Connected
	}

	return s
}

// GetConfig returns the saved config for UI pre-fill.
func (c *Controller) GetConfig() ConfigInfo {
	c.mu.Lock()
	defer c.mu.Unlock()

	info := ConfigInfo{}
	if c.cfg != nil {
		info.TeamKey = c.cfg.TeamKey
		if c.cfg.Client != nil {
			info.ServerAddr = c.cfg.Client.ServerAddr
			info.BindInterface = c.cfg.Client.BindInterface
		}
	}
	return info
}

// Connect starts the client with the given parameters.
func (c *Controller) Connect(req ConnectRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectLocked(req)
}

func (c *Controller) connectLocked(req ConnectRequest) error {
	if c.connected {
		return fmt.Errorf("already connected")
	}

	// Clean up server address.
	serverAddr := strings.TrimSpace(req.ServerAddr)
	serverAddr = strings.TrimPrefix(serverAddr, "http://")
	serverAddr = strings.TrimPrefix(serverAddr, "https://")
	serverAddr = strings.TrimRight(serverAddr, "/")

	if req.BindInterface == "" {
		c.lastError = "请选择网卡"
		return fmt.Errorf("no network interface selected")
	}

	cfg := &config.Config{
		Mode:    "client",
		TeamKey: req.TeamKey,
		Client: &config.ClientConfig{
			ServerAddr:    serverAddr,
			SendMode:      "redundant",
			MTU:           1300,
			DedupWindow:   4096,
			BindInterface: req.BindInterface,
		},
	}

	if err := cfg.Validate(); err != nil {
		c.lastError = "配置校验失败: " + err.Error()
		return fmt.Errorf("config validation failed: %w", err)
	}

	if err := config.SaveConfig(c.cfgPath, cfg); err != nil {
		log.WithError(err).Warn("gui: failed to save config")
	}
	c.cfg = cfg

	cli, err := client.New(cfg)
	if err != nil {
		c.lastError = "客户端创建失败: " + err.Error()
		return fmt.Errorf("failed to create client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancelFunc = cancel
	c.client = cli
	c.connected = true
	c.lastError = ""
	c.lastReq = &req
	c.boundIP = getInterfaceIP(req.BindInterface)

	// Start network change watcher.
	watchCtx, watchCancel := context.WithCancel(context.Background())
	c.stopWatch = watchCancel
	go c.watchNetwork(watchCtx)

	go func() {
		if err := cli.Run(ctx); err != nil {
			log.WithError(err).Error("gui: client stopped with error")
			c.mu.Lock()
			c.lastError = err.Error()
			c.mu.Unlock()
		}
		c.mu.Lock()
		c.connected = false
		c.client = nil
		c.mu.Unlock()
		log.Info("gui: client stopped")
	}()

	log.Infof("gui: client started, NIC=%s (%s)", req.BindInterface, c.boundIP)
	return nil
}

// disconnectLocked stops the running client. Caller must hold c.mu.
func (c *Controller) disconnectLocked() {
	if !c.connected {
		return
	}
	if c.stopWatch != nil {
		c.stopWatch()
		c.stopWatch = nil
	}
	if c.cancelFunc != nil {
		c.cancelFunc()
		c.cancelFunc = nil
	}
}

// Disconnect stops the running client.
func (c *Controller) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnectLocked()
	c.lastReq = nil // user-initiated disconnect, don't auto-reconnect
	return nil
}

// watchNetwork monitors the bound interface for IP changes and auto-reconnects.
func (c *Controller) watchNetwork(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			if !c.connected || c.lastReq == nil {
				c.mu.Unlock()
				return
			}
			currentIP := getInterfaceIP(c.lastReq.BindInterface)
			boundIP := c.boundIP
			req := *c.lastReq
			c.mu.Unlock()

			if currentIP == boundIP {
				continue
			}

			if currentIP == "" {
				log.Warn("gui: NIC went down, waiting for reconnect...")
				continue
			}

			// IP changed — reconnect.
			log.Infof("gui: NIC IP changed %s -> %s, reconnecting...", boundIP, currentIP)

			c.mu.Lock()
			c.disconnectLocked()
			c.mu.Unlock()

			// Wait for old client to fully stop.
			time.Sleep(1 * time.Second)

			c.mu.Lock()
			// Wait until connected becomes false (old client stopped).
			for c.connected {
				c.mu.Unlock()
				time.Sleep(500 * time.Millisecond)
				c.mu.Lock()
			}
			err := c.connectLocked(req)
			c.mu.Unlock()

			if err != nil {
				log.WithError(err).Error("gui: auto-reconnect failed")
			} else {
				log.Info("gui: auto-reconnect successful")
			}
			return // new watchNetwork goroutine started by connectLocked
		}
	}
}

// getInterfaceIP returns the first IPv4 address of the named interface, or "".
func getInterfaceIP(name string) string {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip4 := ip.To4(); ip4 != nil && !(ip4[0] == 169 && ip4[1] == 254) {
			return ip4.String()
		}
	}
	return ""
}
