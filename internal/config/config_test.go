package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig_ServerValid(t *testing.T) {
	yaml := `
mode: server
teamKey: "myteam"
server:
  listenAddr: "0.0.0.0:9800"
  virtualIP: "10.99.0.254/24"
  subnet: "10.99.0.0/24"
`
	cfg, err := LoadConfig(writeTestFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode != "server" {
		t.Errorf("mode = %q, want server", cfg.Mode)
	}
	if cfg.Server.ClientTimeout != 15 {
		t.Errorf("clientTimeout = %d, want 15", cfg.Server.ClientTimeout)
	}
	if cfg.Server.AddrTimeout != 5 {
		t.Errorf("addrTimeout = %d, want 5", cfg.Server.AddrTimeout)
	}
	if cfg.Server.DedupWindow != 4096 {
		t.Errorf("dedupWindow = %d, want 4096", cfg.Server.DedupWindow)
	}
	if cfg.Server.MTU != 1300 {
		t.Errorf("mtu = %d, want 1300", cfg.Server.MTU)
	}
	if cfg.Server.IPPoolFile != "ip_pool.json" {
		t.Errorf("ipPoolFile = %q, want ip_pool.json", cfg.Server.IPPoolFile)
	}
}

func TestLoadConfig_ClientValid(t *testing.T) {
	yaml := `
mode: client
teamKey: "myteam"
client:
  clientID: 1
  serverAddr: "1.2.3.4:9800"
  sendMode: failover
  mtu: 1400
  dedupWindow: 8192
`
	cfg, err := LoadConfig(writeTestFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Client.ClientID != 1 {
		t.Errorf("clientID = %d, want 1", cfg.Client.ClientID)
	}
	if cfg.Client.SendMode != "failover" {
		t.Errorf("sendMode = %q, want failover", cfg.Client.SendMode)
	}
	if cfg.Client.MTU != 1400 {
		t.Errorf("mtu = %d, want 1400", cfg.Client.MTU)
	}
	if cfg.Client.DedupWindow != 8192 {
		t.Errorf("dedupWindow = %d, want 8192", cfg.Client.DedupWindow)
	}
}

func TestLoadConfig_DefaultSendMode(t *testing.T) {
	yaml := `
mode: client
teamKey: "myteam"
client:
  clientID: 1
  serverAddr: "1.2.3.4:9800"
`
	cfg, err := LoadConfig(writeTestFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Client.SendMode != "redundant" {
		t.Errorf("sendMode = %q, want redundant", cfg.Client.SendMode)
	}
}

func TestValidate_MissingMode(t *testing.T) {
	yaml := `
teamKey: "myteam"
server:
  listenAddr: "0.0.0.0:9800"
  virtualIP: "10.99.0.254/24"
  subnet: "10.99.0.0/24"
`
	_, err := LoadConfig(writeTestFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing mode")
	}
}

func TestValidate_MissingTeamKey(t *testing.T) {
	yaml := `
mode: server
server:
  listenAddr: "0.0.0.0:9800"
  virtualIP: "10.99.0.254/24"
  subnet: "10.99.0.0/24"
`
	_, err := LoadConfig(writeTestFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing teamKey")
	}
}

func TestValidate_ClientIDZero(t *testing.T) {
	yaml := `
mode: client
teamKey: "myteam"
client:
  clientID: 0
  serverAddr: "1.2.3.4:9800"
`
	cfg, err := LoadConfig(writeTestFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: clientID=0 should be allowed (auto-generate): %v", err)
	}
	if cfg.Client.ClientID != 0 {
		t.Errorf("clientID = %d, want 0", cfg.Client.ClientID)
	}
}

func TestValidate_MissingServerAddr(t *testing.T) {
	yaml := `
mode: client
teamKey: "myteam"
client:
  clientID: 1
`
	_, err := LoadConfig(writeTestFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing serverAddr")
	}
}

func TestValidate_InvalidVirtualIP(t *testing.T) {
	yaml := `
mode: client
teamKey: "myteam"
client:
  clientID: 1
  serverAddr: "1.2.3.4:9800"
  virtualIP: "not-a-cidr"
`
	_, err := LoadConfig(writeTestFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid virtualIP")
	}
}

func TestValidate_ServerMissingSection(t *testing.T) {
	yaml := `
mode: server
teamKey: "myteam"
`
	_, err := LoadConfig(writeTestFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing server section")
	}
}

func TestValidate_ServerInvalidSubnet(t *testing.T) {
	yaml := `
mode: server
teamKey: "myteam"
server:
  listenAddr: "0.0.0.0:9800"
  virtualIP: "10.99.0.254/24"
  subnet: "bad"
`
	_, err := LoadConfig(writeTestFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid subnet")
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path.yml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}
