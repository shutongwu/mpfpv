package config

import (
	"fmt"
	"net"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration.
type Config struct {
	Mode    string        `yaml:"mode"`    // "client" or "server"
	TeamKey string        `yaml:"teamKey"`
	Client  *ClientConfig `yaml:"client,omitempty"`
	Server  *ServerConfig `yaml:"server,omitempty"`
}

// ClientConfig holds client-specific settings.
type ClientConfig struct {
	ClientID           uint16   `yaml:"clientID"`
	VirtualIP          string   `yaml:"virtualIP"`          // "10.99.0.1/24" or empty for auto
	ServerAddr         string   `yaml:"serverAddr"`         // "203.0.113.1:9800"
	SendMode           string   `yaml:"sendMode"`           // "redundant" | "failover"
	MTU                int      `yaml:"mtu"`                // default 1300
	DedupWindow        int      `yaml:"dedupWindow"`        // default 4096
	ExcludedInterfaces []string `yaml:"excludedInterfaces"`
	WebUI              string   `yaml:"webUI"`              // "127.0.0.1:9801" or empty
}

// ServerConfig holds server-specific settings.
type ServerConfig struct {
	ListenAddr    string `yaml:"listenAddr"`    // "0.0.0.0:9800"
	VirtualIP     string `yaml:"virtualIP"`     // "10.99.0.254/24"
	Subnet        string `yaml:"subnet"`        // "10.99.0.0/24"
	ClientTimeout int    `yaml:"clientTimeout"` // seconds, default 15
	AddrTimeout   int    `yaml:"addrTimeout"`   // seconds, default 5
	DedupWindow   int    `yaml:"dedupWindow"`   // default 4096
	MTU           int    `yaml:"mtu"`           // default 1300
	IPPoolFile    string `yaml:"ipPoolFile"`    // "ip_pool.json"
	WebUI         string `yaml:"webUI"`
}

// LoadConfig reads a YAML config file and returns a parsed Config.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}

	applyDefaults(&cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyDefaults fills in default values for zero-valued fields.
func applyDefaults(cfg *Config) {
	if cfg.Client != nil {
		if cfg.Client.SendMode == "" {
			cfg.Client.SendMode = "redundant"
		}
		if cfg.Client.MTU == 0 {
			cfg.Client.MTU = 1300
		}
		if cfg.Client.DedupWindow == 0 {
			cfg.Client.DedupWindow = 4096
		}
	}

	if cfg.Server != nil {
		if cfg.Server.ClientTimeout == 0 {
			cfg.Server.ClientTimeout = 15
		}
		if cfg.Server.AddrTimeout == 0 {
			cfg.Server.AddrTimeout = 5
		}
		if cfg.Server.DedupWindow == 0 {
			cfg.Server.DedupWindow = 4096
		}
		if cfg.Server.MTU == 0 {
			cfg.Server.MTU = 1300
		}
		if cfg.Server.IPPoolFile == "" {
			cfg.Server.IPPoolFile = "ip_pool.json"
		}
	}
}

// Validate checks required fields and formats.
func (c *Config) Validate() error {
	if c.Mode != "client" && c.Mode != "server" {
		return fmt.Errorf("config: mode must be \"client\" or \"server\", got %q", c.Mode)
	}

	if c.TeamKey == "" {
		return fmt.Errorf("config: teamKey is required")
	}

	switch c.Mode {
	case "client":
		if c.Client == nil {
			return fmt.Errorf("config: client section is required when mode is \"client\"")
		}
		if err := c.Client.validate(); err != nil {
			return err
		}
	case "server":
		if c.Server == nil {
			return fmt.Errorf("config: server section is required when mode is \"server\"")
		}
		if err := c.Server.validate(); err != nil {
			return err
		}
	}

	return nil
}

func (cc *ClientConfig) validate() error {
	if cc.ClientID == 0 {
		return fmt.Errorf("config: client.clientID must be > 0 (0 is reserved for server)")
	}

	if cc.ServerAddr == "" {
		return fmt.Errorf("config: client.serverAddr is required")
	}
	if _, _, err := net.SplitHostPort(cc.ServerAddr); err != nil {
		return fmt.Errorf("config: client.serverAddr invalid: %w", err)
	}

	if cc.VirtualIP != "" {
		if _, _, err := net.ParseCIDR(cc.VirtualIP); err != nil {
			return fmt.Errorf("config: client.virtualIP invalid CIDR: %w", err)
		}
	}

	if cc.SendMode != "redundant" && cc.SendMode != "failover" {
		return fmt.Errorf("config: client.sendMode must be \"redundant\" or \"failover\", got %q", cc.SendMode)
	}

	if cc.MTU < 576 || cc.MTU > 9000 {
		return fmt.Errorf("config: client.mtu must be between 576 and 9000, got %d", cc.MTU)
	}

	return nil
}

func (sc *ServerConfig) validate() error {
	if sc.ListenAddr == "" {
		return fmt.Errorf("config: server.listenAddr is required")
	}
	if _, _, err := net.SplitHostPort(sc.ListenAddr); err != nil {
		return fmt.Errorf("config: server.listenAddr invalid: %w", err)
	}

	if sc.VirtualIP == "" {
		return fmt.Errorf("config: server.virtualIP is required")
	}
	if _, _, err := net.ParseCIDR(sc.VirtualIP); err != nil {
		return fmt.Errorf("config: server.virtualIP invalid CIDR: %w", err)
	}

	if sc.Subnet == "" {
		return fmt.Errorf("config: server.subnet is required")
	}
	if _, _, err := net.ParseCIDR(sc.Subnet); err != nil {
		return fmt.Errorf("config: server.subnet invalid CIDR: %w", err)
	}

	if sc.MTU < 576 || sc.MTU > 9000 {
		return fmt.Errorf("config: server.mtu must be between 576 and 9000, got %d", sc.MTU)
	}

	return nil
}
