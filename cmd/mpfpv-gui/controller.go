//go:build windows

package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloud/mpfpv/internal/client"
	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/web"
	log "github.com/sirupsen/logrus"
)

// Controller implements web.GUIController and manages the client lifecycle.
type Controller struct {
	cfg        *config.Config
	cfgPath    string
	client     *client.Client
	cancelFunc context.CancelFunc
	mu         sync.Mutex
	connected  bool
	lastError  string

	// onClientReady is called when a client is created and connected,
	// so the web handler can serve client-specific API endpoints.
	onClientReady func(capi web.ClientAPI)
	onClientStop  func()
}

// NewController creates a new GUI controller.
func NewController(cfgPath string, cfg *config.Config) *Controller {
	return &Controller{
		cfg:     cfg,
		cfgPath: cfgPath,
	}
}

// GetConfig returns the current configuration.
func (c *Controller) GetConfig() *config.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// SaveConfig validates and saves the configuration to disk.
func (c *Controller) SaveConfig(cfg *config.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Force mode to client for the GUI.
	cfg.Mode = "client"

	// Apply defaults before validation.
	if cfg.Client != nil {
		if cfg.Client.SendMode == "" {
			cfg.Client.SendMode = "redundant"
		}
		cfg.Client.MTU = 1400 // fixed
		if cfg.Client.DedupWindow == 0 {
			cfg.Client.DedupWindow = 4096
		}
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}

	if err := config.SaveConfig(c.cfgPath, cfg); err != nil {
		return err
	}

	c.cfg = cfg
	log.Info("gui: config saved to ", c.cfgPath)
	return nil
}

// Connect starts the mpfpv client with the current configuration.
func (c *Controller) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return fmt.Errorf("already connected")
	}

	if c.cfg == nil || c.cfg.Client == nil {
		return fmt.Errorf("no valid client configuration")
	}

	cli, err := client.New(c.cfg)
	if err != nil {
		c.lastError = err.Error()
		return fmt.Errorf("failed to create client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancelFunc = cancel
	c.client = cli
	c.connected = true
	c.lastError = ""

	// Notify handler that a ClientAPI is available.
	if c.onClientReady != nil {
		c.onClientReady(cli)
	}

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
		if c.onClientStop != nil {
			c.onClientStop()
		}
		log.Info("gui: client stopped")
	}()

	log.Info("gui: client started")
	return nil
}

// Disconnect stops the running client.
func (c *Controller) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return nil
	}

	if c.cancelFunc != nil {
		c.cancelFunc()
		c.cancelFunc = nil
	}

	// The goroutine in Connect() will set connected=false when Run() returns.
	return nil
}

// IsConnected returns whether the client is currently connected.
func (c *Controller) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// GetClient returns the current client instance (may be nil).
func (c *Controller) GetClient() *client.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
}
