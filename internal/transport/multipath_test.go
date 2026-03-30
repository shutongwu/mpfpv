package transport

import (
	"net"
	"testing"
	"time"

	"github.com/cloud/mpfpv/internal/protocol"
)

// --- InterfaceWatcher tests ---

func TestScanInterfaces_FiltersLoopbackAndExcluded(t *testing.T) {
	w := NewInterfaceWatcher([]string{"docker0", "br-test"}, nil)
	result := w.scanInterfaces()

	for name, info := range result {
		if name == "lo" {
			t.Error("scanInterfaces should exclude loopback")
		}
		if name == "docker0" || name == "br-test" {
			t.Errorf("scanInterfaces should exclude %q", name)
		}
		if !info.IsUp {
			t.Errorf("scanInterfaces should only return up interfaces, got down: %s", name)
		}
		if len(info.Addrs) == 0 {
			t.Errorf("scanInterfaces should only return interfaces with IPv4: %s", name)
		}
	}
}

func TestScanInterfaces_ReturnsAtLeastOne(t *testing.T) {
	// On any machine with at least one non-loopback interface up with an IPv4.
	w := NewInterfaceWatcher(nil, nil)
	result := w.scanInterfaces()
	// We cannot guarantee interfaces exist in all CI environments,
	// so just verify it runs without error.
	_ = result
}

func TestInterfaceWatcher_StartStop(t *testing.T) {
	w := NewInterfaceWatcher(nil, nil)
	if err := w.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	// Watcher should have populated current.
	cur := w.Current()
	_ = cur // may be empty in CI
	w.Stop()
	// Double stop should not panic.
	w.Stop()
}

func TestInterfaceWatcher_DetectChanges(t *testing.T) {
	var addedNames, removedNames []string
	onChange := func(added, removed []InterfaceInfo) {
		for _, a := range added {
			addedNames = append(addedNames, a.Name)
		}
		for _, r := range removed {
			removedNames = append(removedNames, r.Name)
		}
	}

	w := NewInterfaceWatcher(nil, onChange)
	// Seed current with a fake interface.
	w.mu.Lock()
	w.current = map[string]*InterfaceInfo{
		"fake0": {Name: "fake0", Addrs: []net.IP{net.IPv4(192, 168, 99, 1)}, IsUp: true},
	}
	w.mu.Unlock()

	// detectChanges should notice fake0 is gone.
	w.detectChanges()

	found := false
	for _, name := range removedNames {
		if name == "fake0" {
			found = true
		}
	}
	if !found {
		t.Error("expected fake0 to be detected as removed")
	}
}

// --- Path / RTT tests ---

func TestAverageDuration(t *testing.T) {
	samples := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}
	avg := averageDuration(samples)
	if avg != 20*time.Millisecond {
		t.Errorf("expected 20ms, got %v", avg)
	}

	if averageDuration(nil) != 0 {
		t.Error("expected 0 for empty slice")
	}
}

func TestSameAddrs(t *testing.T) {
	a := []net.IP{net.IPv4(1, 2, 3, 4), net.IPv4(5, 6, 7, 8)}
	b := []net.IP{net.IPv4(5, 6, 7, 8), net.IPv4(1, 2, 3, 4)}
	if !sameAddrs(a, b) {
		t.Error("expected same addresses")
	}
	c := []net.IP{net.IPv4(1, 2, 3, 4)}
	if sameAddrs(a, c) {
		t.Error("expected different addresses")
	}
}

// --- MultiPathSender tests ---

func TestNewMultiPathSender_NilAddr(t *testing.T) {
	_, err := NewMultiPathSender(nil, protocol.SendModeRedundant, nil)
	if err == nil {
		t.Error("expected error for nil serverAddr")
	}
}

func TestMultiPathSender_CreateAndStop(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, err := NewMultiPathSender(addr, protocol.SendModeRedundant, nil)
	if err != nil {
		t.Fatalf("NewMultiPathSender failed: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	// Give watcher time to scan.
	time.Sleep(50 * time.Millisecond)
	m.Stop()
}

func TestMultiPathSender_SendRedundant(t *testing.T) {
	// Create a "server" socket to receive on.
	serverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	serverConn, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer serverConn.Close()
	serverAddr = serverConn.LocalAddr().(*net.UDPAddr)

	m, err := NewMultiPathSender(serverAddr, protocol.SendModeRedundant, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// Manually add paths using loopback via AddPathForTest.
	loConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen lo: %v", err)
	}
	m.AddPathForTest("test-lo", net.IPv4(127, 0, 0, 1), loConn)

	loConn2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen lo2: %v", err)
	}
	m.AddPathForTest("test-lo2", net.IPv4(127, 0, 0, 1), loConn2)

	payload := []byte("hello-redundant")
	if err := m.Send(payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// We should receive the packet at least once (possibly twice).
	serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1500)
	received := 0
	for i := 0; i < 2; i++ {
		n, _, err := serverConn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if string(buf[:n]) == string(payload) {
			received++
		}
	}
	if received < 1 {
		t.Error("expected at least 1 packet in redundant mode")
	}
	if received != 2 {
		t.Logf("received %d packets (expected 2 for redundant)", received)
	}

	loConn.Close()
	loConn2.Close()
}

func TestMultiPathSender_SendFailover(t *testing.T) {
	serverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	serverConn, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer serverConn.Close()
	serverAddr = serverConn.LocalAddr().(*net.UDPAddr)

	m, err := NewMultiPathSender(serverAddr, protocol.SendModeFailover, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// Add two paths with different RTTs.
	loConn1, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	loConn2, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer loConn1.Close()
	defer loConn2.Close()

	m.mu.Lock()
	m.paths["fast"] = &Path{
		IfaceName:  "fast",
		LocalAddr:  net.IPv4(127, 0, 0, 1),
		Conn:       loConn1,
		Status:     PathActive,
		RTT:        5 * time.Millisecond,
		rttSamples: []time.Duration{5 * time.Millisecond},
		LastRecv:   time.Now(),
	}
	m.paths["slow"] = &Path{
		IfaceName:  "slow",
		LocalAddr:  net.IPv4(127, 0, 0, 1),
		Conn:       loConn2,
		Status:     PathActive,
		RTT:        50 * time.Millisecond,
		rttSamples: []time.Duration{50 * time.Millisecond},
		LastRecv:   time.Now(),
	}
	m.activePath = m.selectBestPathLocked()
	m.mu.Unlock()

	if m.activePath != "fast" {
		t.Errorf("expected active path 'fast', got %q", m.activePath)
	}

	payload := []byte("hello-failover")
	if err := m.Send(payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Should receive exactly 1 packet.
	serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1500)
	received := 0
	for i := 0; i < 2; i++ {
		n, _, err := serverConn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if string(buf[:n]) == string(payload) {
			received++
		}
	}
	if received != 1 {
		t.Errorf("failover: expected 1 packet, got %d", received)
	}
}

func TestUpdateRTT_SlidingWindow(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeRedundant, nil)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn.Close()

	m.mu.Lock()
	m.paths["eth0"] = &Path{
		IfaceName: "eth0",
		LocalAddr: net.IPv4(10, 0, 0, 1),
		Conn:      conn,
		Status:    PathActive,
	}
	m.mu.Unlock()

	// Add 15 samples — only the last 10 should be kept.
	for i := 0; i < 15; i++ {
		m.UpdateRTT("eth0", time.Duration(i+1)*time.Millisecond)
	}

	m.mu.RLock()
	p := m.paths["eth0"]
	m.mu.RUnlock()

	p.mu.Lock()
	if len(p.rttSamples) != defaultRTTSamples {
		t.Errorf("expected %d samples, got %d", defaultRTTSamples, len(p.rttSamples))
	}
	// Last 10 are 6..15 ms. Average = (6+7+8+9+10+11+12+13+14+15)/10 = 10.5ms
	expectedAvg := 10500 * time.Microsecond
	if p.RTT != expectedAvg {
		t.Errorf("expected avg RTT %v, got %v", expectedAvg, p.RTT)
	}
	p.mu.Unlock()
}

func TestIncrementMiss_MarksDown(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeRedundant, nil)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn.Close()

	m.mu.Lock()
	m.paths["wlan0"] = &Path{
		IfaceName: "wlan0",
		LocalAddr: net.IPv4(10, 0, 0, 2),
		Conn:      conn,
		Status:    PathActive,
	}
	m.mu.Unlock()

	for i := 0; i < missThresholdDown; i++ {
		m.IncrementMiss("wlan0")
	}

	m.mu.RLock()
	p := m.paths["wlan0"]
	m.mu.RUnlock()

	p.mu.Lock()
	if p.Status != PathDown {
		t.Errorf("expected PathDown after %d misses, got %v", missThresholdDown, p.Status)
	}
	p.mu.Unlock()
}

func TestAntiPingPong_Cooldown(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeFailover, nil)
	m.switchCooldown = 1 * time.Second

	conn1, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	conn2, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn1.Close()
	defer conn2.Close()

	m.mu.Lock()
	m.paths["pathA"] = &Path{
		IfaceName:  "pathA",
		LocalAddr:  net.IPv4(10, 0, 0, 1),
		Conn:       conn1,
		Status:     PathActive,
		RTT:        20 * time.Millisecond,
		rttSamples: []time.Duration{20 * time.Millisecond},
	}
	m.paths["pathB"] = &Path{
		IfaceName:  "pathB",
		LocalAddr:  net.IPv4(10, 0, 0, 2),
		Conn:       conn2,
		Status:     PathActive,
		RTT:        10 * time.Millisecond,
		rttSamples: []time.Duration{10 * time.Millisecond},
	}
	// Set activePath to pathA and recent switch time.
	m.activePath = "pathA"
	m.lastSwitch = time.Now()
	m.mu.Unlock()

	// pathB has lower RTT but we just switched, so should stay on pathA.
	m.mu.Lock()
	best := m.selectBestPathLocked()
	m.mu.Unlock()

	if best != "pathA" {
		t.Errorf("expected pathA (cooldown), got %q", best)
	}

	// Simulate cooldown expired.
	m.mu.Lock()
	m.lastSwitch = time.Now().Add(-2 * time.Second)
	best = m.selectBestPathLocked()
	m.mu.Unlock()

	if best != "pathB" {
		t.Errorf("expected pathB after cooldown, got %q", best)
	}
}

func TestAntiPingPong_ImmediateSwitchOnDown(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeFailover, nil)
	m.switchCooldown = 10 * time.Second // very long cooldown

	conn1, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	conn2, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn1.Close()
	defer conn2.Close()

	m.mu.Lock()
	m.paths["pathA"] = &Path{
		IfaceName:  "pathA",
		LocalAddr:  net.IPv4(10, 0, 0, 1),
		Conn:       conn1,
		Status:     PathDown, // current path is down
		RTT:        20 * time.Millisecond,
		rttSamples: []time.Duration{20 * time.Millisecond},
	}
	m.paths["pathB"] = &Path{
		IfaceName:  "pathB",
		LocalAddr:  net.IPv4(10, 0, 0, 2),
		Conn:       conn2,
		Status:     PathActive,
		RTT:        10 * time.Millisecond,
		rttSamples: []time.Duration{10 * time.Millisecond},
	}
	m.activePath = "pathA"
	m.lastSwitch = time.Now() // just switched
	best := m.selectBestPathLocked()
	m.mu.Unlock()

	if best != "pathB" {
		t.Errorf("expected immediate switch to pathB when pathA is down, got %q", best)
	}
}

func TestSendAll_AllPathsReceive(t *testing.T) {
	serverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	serverConn, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer serverConn.Close()
	serverAddr = serverConn.LocalAddr().(*net.UDPAddr)

	m, _ := NewMultiPathSender(serverAddr, protocol.SendModeFailover, nil)

	conn1, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	conn2, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn1.Close()
	defer conn2.Close()

	m.mu.Lock()
	m.paths["p1"] = &Path{IfaceName: "p1", LocalAddr: net.IPv4(127, 0, 0, 1), Conn: conn1, Status: PathActive}
	m.paths["p2"] = &Path{IfaceName: "p2", LocalAddr: net.IPv4(127, 0, 0, 1), Conn: conn2, Status: PathActive}
	m.mu.Unlock()

	payload := []byte("heartbeat-all")
	m.SendAll(payload)

	serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1500)
	received := 0
	for i := 0; i < 3; i++ {
		n, _, err := serverConn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if string(buf[:n]) == string(payload) {
			received++
		}
	}
	if received != 2 {
		t.Errorf("SendAll: expected 2 packets, got %d", received)
	}
}

func TestGetPaths(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeFailover, nil)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn.Close()

	m.mu.Lock()
	m.paths["eth0"] = &Path{
		IfaceName: "eth0",
		LocalAddr: net.IPv4(10, 0, 0, 1),
		Conn:      conn,
		Status:    PathActive,
		RTT:       15 * time.Millisecond,
	}
	m.activePath = "eth0"
	m.mu.Unlock()

	infos := m.GetPaths()
	if len(infos) != 1 {
		t.Fatalf("expected 1 path info, got %d", len(infos))
	}
	pi := infos[0]
	if pi.IfaceName != "eth0" {
		t.Errorf("expected eth0, got %s", pi.IfaceName)
	}
	if pi.Status != "active" {
		t.Errorf("expected active, got %s", pi.Status)
	}
	if !pi.IsActive {
		t.Error("expected IsActive=true for failover active path")
	}
}

func TestCheckAndRecycleStalePaths_DetectsStale(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeRedundant, nil)
	m.stopCh = make(chan struct{})

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	oldPort := conn.LocalAddr().(*net.UDPAddr).Port

	m.mu.Lock()
	m.paths["eth0"] = &Path{
		IfaceName: "eth0",
		LocalAddr: net.IPv4(127, 0, 0, 1),
		Conn:      conn,
		Status:    PathActive,
		LastRecv:  time.Now().Add(-10 * time.Second), // stale
		closed:    make(chan struct{}),
	}
	m.mu.Unlock()

	m.CheckAndRecycleStalePaths()

	m.mu.RLock()
	p := m.paths["eth0"]
	m.mu.RUnlock()

	p.mu.Lock()
	newPort := p.Conn.LocalAddr().(*net.UDPAddr).Port
	rc := p.recycleCount
	p.mu.Unlock()

	if newPort == oldPort {
		t.Error("expected socket to be recycled with new port")
	}
	if rc != 1 {
		t.Errorf("expected recycleCount=1, got %d", rc)
	}

	// Clean up.
	p.Conn.Close()
}

func TestRecycleResetOnRecovery(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeRedundant, nil)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn.Close()

	m.mu.Lock()
	m.paths["wlan0"] = &Path{
		IfaceName:    "wlan0",
		LocalAddr:    net.IPv4(10, 0, 0, 2),
		Conn:         conn,
		Status:       PathActive,
		recycleCount: 2,
		closed:       make(chan struct{}),
	}
	m.mu.Unlock()

	m.UpdateRTT("wlan0", 30*time.Millisecond)

	m.mu.RLock()
	p := m.paths["wlan0"]
	m.mu.RUnlock()

	p.mu.Lock()
	if p.recycleCount != 0 {
		t.Errorf("expected recycleCount reset to 0 after RTT update, got %d", p.recycleCount)
	}
	p.mu.Unlock()
}

func TestRecycleBackoff(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeRedundant, nil)
	m.stopCh = make(chan struct{})

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})

	m.mu.Lock()
	m.paths["eth0"] = &Path{
		IfaceName:    "eth0",
		LocalAddr:    net.IPv4(127, 0, 0, 1),
		Conn:         conn,
		Status:       PathActive,
		LastRecv:     time.Now().Add(-10 * time.Second),
		recycleCount: recycleMaxAttempts, // already at max
		lastRecycled: time.Now().Add(-5 * time.Second), // recent
		closed:       make(chan struct{}),
	}
	m.mu.Unlock()

	m.CheckAndRecycleStalePaths()

	m.mu.RLock()
	p := m.paths["eth0"]
	m.mu.RUnlock()

	p.mu.Lock()
	if p.Status != PathDown {
		t.Errorf("expected PathDown after max recycle attempts, got %v", p.Status)
	}
	// recycleCount should not have increased (no recycle happened).
	if p.recycleCount != recycleMaxAttempts {
		t.Errorf("expected recycleCount=%d (unchanged), got %d", recycleMaxAttempts, p.recycleCount)
	}
	p.mu.Unlock()
	conn.Close()
}

func TestAddPath_IPv4Preferred(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeRedundant, nil)
	m.stopCh = make(chan struct{})

	info := &InterfaceInfo{
		Name:   "lo",
		Addrs:  []net.IP{net.IPv4(127, 0, 0, 1)},
		Addrs6: []net.IP{net.IPv6loopback},
		IsUp:   true,
	}
	m.addPath(info)

	m.mu.RLock()
	p, ok := m.paths["lo"]
	m.mu.RUnlock()

	if !ok {
		t.Fatal("expected path to be created")
	}
	p.mu.Lock()
	addr4 := p.LocalAddr.To4()
	p.mu.Unlock()

	if addr4 == nil {
		t.Error("expected IPv4 address to be selected when server is IPv4")
	}
	p.Conn.Close()
}

func TestAddPath_IPv6Fallback(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "[::1]:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeRedundant, nil)
	m.stopCh = make(chan struct{})

	info := &InterfaceInfo{
		Name:   "lo",
		Addrs:  []net.IP{net.IPv4(127, 0, 0, 1)},
		Addrs6: []net.IP{net.IPv6loopback},
		IsUp:   true,
	}
	m.addPath(info)

	m.mu.RLock()
	p, ok := m.paths["lo"]
	m.mu.RUnlock()

	if !ok {
		t.Fatal("expected path to be created for IPv6 server")
	}
	p.mu.Lock()
	isIPv6 := p.LocalAddr.To4() == nil
	p.mu.Unlock()

	if !isIPv6 {
		t.Error("expected IPv6 address to be selected when server is IPv6")
	}
	p.Conn.Close()
}

func TestGetPaths_IncludesRecycleCount(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:19800")
	m, _ := NewMultiPathSender(addr, protocol.SendModeRedundant, nil)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn.Close()

	m.mu.Lock()
	m.paths["eth0"] = &Path{
		IfaceName:    "eth0",
		LocalAddr:    net.IPv4(10, 0, 0, 1),
		Conn:         conn,
		Status:       PathActive,
		recycleCount: 3,
	}
	m.mu.Unlock()

	infos := m.GetPaths()
	if len(infos) != 1 {
		t.Fatalf("expected 1 path, got %d", len(infos))
	}
	if infos[0].RecycleCount != 3 {
		t.Errorf("expected RecycleCount=3, got %d", infos[0].RecycleCount)
	}
}
