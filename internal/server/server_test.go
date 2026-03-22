package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/protocol"
)

func testConfig() *config.Config {
	return &config.Config{
		Mode:    "server",
		TeamKey: "testkey",
		Server: &config.ServerConfig{
			ListenAddr:    "127.0.0.1:0",
			VirtualIP:     "10.99.0.254/24",
			Subnet:        "10.99.0.0/24",
			ClientTimeout: 15,
			AddrTimeout:   5,
			DedupWindow:   4096,
			MTU:           1300,
		},
	}
}

func startTestServer(t *testing.T) (*Server, *net.UDPConn, func()) {
	t.Helper()
	cfg := testConfig()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Bind to a random port.
	addr, _ := net.ResolveUDPAddr("udp4", "127.0.0.1:0")
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.conn = conn

	return s, conn, func() { conn.Close() }
}

func buildHeartbeat(clientID uint16, vip net.IP, prefixLen uint8, sendMode uint8, teamKey string) []byte {
	buf := make([]byte, protocol.HeaderSize+protocol.HeartbeatPayloadSize)
	protocol.EncodeHeader(buf, &protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeHeartbeat,
		ClientID: clientID,
		Seq:      1,
	})
	hash := protocol.ComputeTeamKeyHash(teamKey)
	protocol.EncodeHeartbeat(buf[protocol.HeaderSize:], &protocol.HeartbeatPayload{
		VirtualIP:   vip,
		PrefixLen:   prefixLen,
		SendMode:    sendMode,
		TeamKeyHash: hash,
	})
	return buf
}

func buildDataPacket(clientID uint16, seq uint32, srcIP, dstIP net.IP) []byte {
	// Build a minimal fake IPv4 header (20 bytes) + mpfpv header.
	ipHeader := make([]byte, 20)
	ipHeader[0] = 0x45 // version=4, IHL=5
	copy(ipHeader[12:16], srcIP.To4())
	copy(ipHeader[16:20], dstIP.To4())

	buf := make([]byte, protocol.HeaderSize+len(ipHeader))
	protocol.EncodeHeader(buf, &protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeData,
		ClientID: clientID,
		Seq:      seq,
	})
	copy(buf[protocol.HeaderSize:], ipHeader)
	return buf
}

// drainHeartbeatAcks reads and discards any HeartbeatAck packets from the connection.
func drainHeartbeatAcks(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	buf := make([]byte, 2000)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return // no more packets
		}
		if n >= protocol.HeaderSize {
			hdr, err := protocol.DecodeHeader(buf[:n])
			if err == nil && hdr.Type == protocol.TypeHeartbeatAck {
				continue // discard ack
			}
		}
	}
}

// --- mockTUN for testing ---

type mockTUN struct {
	readBuf  chan []byte
	writeBuf chan []byte
	name     string
	closed   bool
	mu       sync.Mutex
}

func newMockTUN(name string) *mockTUN {
	return &mockTUN{
		readBuf:  make(chan []byte, 100),
		writeBuf: make(chan []byte, 100),
		name:     name,
	}
}

func (m *mockTUN) Read(buf []byte) (int, error) {
	data, ok := <-m.readBuf
	if !ok {
		return 0, errors.New("mockTUN: closed")
	}
	n := copy(buf, data)
	return n, nil
}

func (m *mockTUN) Write(buf []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, errors.New("mockTUN: closed")
	}
	cp := make([]byte, len(buf))
	copy(cp, buf)
	m.writeBuf <- cp
	return len(buf), nil
}

func (m *mockTUN) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.readBuf)
	}
	return nil
}

func (m *mockTUN) Name() string {
	return m.name
}

// --- Heartbeat tests ---

func TestHeartbeat_NormalRegistration(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	pkt := buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey")

	s.handlePacket(pkt, from)

	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	session, ok := s.sessions[1]
	if !ok {
		t.Fatal("session not created")
	}
	if !session.VirtualIP.Equal(net.ParseIP("10.99.0.1")) {
		t.Errorf("virtualIP = %v, want 10.99.0.1", session.VirtualIP)
	}
	if session.SendMode != protocol.SendModeRedundant {
		t.Errorf("sendMode = %d, want redundant", session.SendMode)
	}
	if len(session.Addrs) != 1 {
		t.Errorf("addrs count = %d, want 1", len(session.Addrs))
	}

	// Check route table.
	s.routeLock.RLock()
	defer s.routeLock.RUnlock()
	var key [4]byte
	copy(key[:], net.ParseIP("10.99.0.1").To4())
	cid, ok := s.routeTable[key]
	if !ok {
		t.Fatal("route not created")
	}
	if cid != 1 {
		t.Errorf("route clientID = %d, want 1", cid)
	}
}

func TestHeartbeat_TeamKeyMismatch(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	pkt := buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "wrongkey")

	s.handlePacket(pkt, from)

	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	if _, ok := s.sessions[1]; ok {
		t.Fatal("session should not be created for mismatched teamKey")
	}
}

func TestHeartbeat_ClientIDConflict(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}

	// Register clientID=1 with IP 10.99.0.1
	pkt1 := buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey")
	s.handlePacket(pkt1, from)

	// Try to register clientID=1 with a different IP 10.99.0.2
	from2 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12346}
	pkt2 := buildHeartbeat(1, net.ParseIP("10.99.0.2"), 24, protocol.SendModeRedundant, "testkey")
	s.handlePacket(pkt2, from2)

	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	session := s.sessions[1]
	// Virtual IP should still be the original.
	if !session.VirtualIP.Equal(net.ParseIP("10.99.0.1")) {
		t.Errorf("virtualIP changed to %v, should remain 10.99.0.1", session.VirtualIP)
	}
}

func TestHeartbeat_MultipleAddrs(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	pkt := buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey")

	from1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	from2 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12346}

	s.handlePacket(pkt, from1)
	s.handlePacket(pkt, from2)

	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	if len(s.sessions[1].Addrs) != 2 {
		t.Errorf("addrs count = %d, want 2", len(s.sessions[1].Addrs))
	}
}

// --- Data forwarding tests ---

func TestData_NormalForward(t *testing.T) {
	s, serverConn, cleanup := startTestServer(t)
	defer cleanup()

	// Register client 1 (10.99.0.1).
	from1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 20001}
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), from1)

	// Register client 2 (10.99.0.2) — need a real UDP socket to receive.
	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	from2 := clientConn.LocalAddr().(*net.UDPAddr)
	s.handlePacket(buildHeartbeat(2, net.ParseIP("10.99.0.2"), 24, protocol.SendModeRedundant, "testkey"), from2)

	// Drain the HeartbeatAck that was sent to client 2 during registration.
	drainHeartbeatAcks(t, clientConn)

	// Client 1 sends data to client 2.
	dataPkt := buildDataPacket(1, 100, net.ParseIP("10.99.0.1"), net.ParseIP("10.99.0.2"))
	s.handlePacket(dataPkt, from1)

	// Client 2 should receive the packet.
	_ = clientConn.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf := make([]byte, 2000)
	n, recvFrom, err := clientConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("client 2 did not receive packet: %v", err)
	}

	// Verify it came from server.
	if recvFrom.Port != serverConn.LocalAddr().(*net.UDPAddr).Port {
		t.Errorf("packet came from port %d, expected server port %d", recvFrom.Port, serverConn.LocalAddr().(*net.UDPAddr).Port)
	}

	// Verify header.
	hdr, err := protocol.DecodeHeader(buf[:n])
	if err != nil {
		t.Fatalf("decode forwarded header: %v", err)
	}
	if hdr.ClientID != 1 {
		t.Errorf("forwarded clientID = %d, want 1", hdr.ClientID)
	}
	if hdr.Seq != 100 {
		t.Errorf("forwarded seq = %d, want 100", hdr.Seq)
	}
}

func TestData_Dedup(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	// Register clients.
	from1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 20001}
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), from1)

	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	from2 := clientConn.LocalAddr().(*net.UDPAddr)
	s.handlePacket(buildHeartbeat(2, net.ParseIP("10.99.0.2"), 24, protocol.SendModeRedundant, "testkey"), from2)

	// Drain HeartbeatAck.
	drainHeartbeatAcks(t, clientConn)

	// Send same data packet twice (simulating redundant send).
	dataPkt := buildDataPacket(1, 200, net.ParseIP("10.99.0.1"), net.ParseIP("10.99.0.2"))
	s.handlePacket(dataPkt, from1)
	s.handlePacket(dataPkt, from1) // duplicate

	// Should only receive one.
	_ = clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 2000)
	_, _, err = clientConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("should receive first packet: %v", err)
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = clientConn.ReadFromUDP(buf)
	if err == nil {
		t.Fatal("should not receive duplicate packet")
	}
}

func TestData_InnerIPMismatch(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	// Register client 1 as 10.99.0.1.
	from1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 20001}
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), from1)

	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	from2 := clientConn.LocalAddr().(*net.UDPAddr)
	s.handlePacket(buildHeartbeat(2, net.ParseIP("10.99.0.2"), 24, protocol.SendModeRedundant, "testkey"), from2)

	// Drain HeartbeatAck.
	drainHeartbeatAcks(t, clientConn)

	// Client 1 sends data with wrong inner source IP (10.99.0.99 instead of 10.99.0.1).
	dataPkt := buildDataPacket(1, 300, net.ParseIP("10.99.0.99"), net.ParseIP("10.99.0.2"))
	s.handlePacket(dataPkt, from1)

	// Client 2 should NOT receive the packet.
	_ = clientConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 2000)
	_, _, err = clientConn.ReadFromUDP(buf)
	if err == nil {
		t.Fatal("should not receive packet with mismatched inner IP")
	}
}

// --- Timeout cleanup tests ---

func TestCleanup_AddrTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.Server.AddrTimeout = 1 // 1 second for test
	cfg.Server.ClientTimeout = 10

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s.conn = conn

	// Register with two addresses.
	from1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 30001}
	from2 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 30002}
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), from1)
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), from2)

	s.sessionsLock.RLock()
	if len(s.sessions[1].Addrs) != 2 {
		t.Fatalf("expected 2 addrs, got %d", len(s.sessions[1].Addrs))
	}
	s.sessionsLock.RUnlock()

	// Wait for addr timeout.
	time.Sleep(1500 * time.Millisecond)
	s.cleanup()

	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	session, ok := s.sessions[1]
	if !ok {
		t.Fatal("session should still exist (clientTimeout not reached)")
	}
	if len(session.Addrs) != 0 {
		t.Errorf("addrs count = %d, want 0 (should be timed out)", len(session.Addrs))
	}
}

func TestCleanup_SessionTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.Server.AddrTimeout = 1
	cfg.Server.ClientTimeout = 2

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s.conn = conn

	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 30001}
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), from)

	// Wait for both addr and client timeout.
	time.Sleep(2500 * time.Millisecond)
	s.cleanup()

	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	if _, ok := s.sessions[1]; ok {
		t.Fatal("session should be removed after clientTimeout")
	}

	s.routeLock.RLock()
	defer s.routeLock.RUnlock()
	var key [4]byte
	copy(key[:], net.ParseIP("10.99.0.1").To4())
	if _, ok := s.routeTable[key]; ok {
		t.Fatal("route should be removed after session timeout")
	}
}

// --- sendToClient tests ---

func TestSendToClient_Redundant(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	// Create two receivers for the same clientID.
	recv1, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer recv1.Close()

	recv2, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer recv2.Close()

	// Register client with both addresses.
	addr1 := recv1.LocalAddr().(*net.UDPAddr)
	addr2 := recv2.LocalAddr().(*net.UDPAddr)

	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), addr1)
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), addr2)

	// Drain HeartbeatAcks.
	drainHeartbeatAcks(t, recv1)
	drainHeartbeatAcks(t, recv2)

	// Send a packet to client 1.
	testData := []byte("testpayload")
	s.sendToClient(1, testData)

	// Both receivers should get the packet.
	buf := make([]byte, 100)

	_ = recv1.SetReadDeadline(time.Now().Add(1 * time.Second))
	n1, _, err := recv1.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("recv1 should receive: %v", err)
	}
	if string(buf[:n1]) != "testpayload" {
		t.Errorf("recv1 got %q", buf[:n1])
	}

	_ = recv2.SetReadDeadline(time.Now().Add(1 * time.Second))
	n2, _, err := recv2.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("recv2 should receive: %v", err)
	}
	if string(buf[:n2]) != "testpayload" {
		t.Errorf("recv2 got %q", buf[:n2])
	}
}

func TestSendToClient_Failover(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	recv1, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer recv1.Close()

	recv2, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer recv2.Close()

	addr1 := recv1.LocalAddr().(*net.UDPAddr)
	addr2 := recv2.LocalAddr().(*net.UDPAddr)

	// Register with failover mode via both addresses.
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeFailover, "testkey"), addr1)
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeFailover, "testkey"), addr2)

	// Drain HeartbeatAcks.
	drainHeartbeatAcks(t, recv1)
	drainHeartbeatAcks(t, recv2)

	// Simulate a data packet from addr2 so server learns the active path.
	dataFromClient := make([]byte, protocol.HeaderSize+28)
	protocol.EncodeHeader(dataFromClient, &protocol.Header{
		Version: protocol.Version1, Type: protocol.TypeData, ClientID: 1, Seq: 100,
	})
	// Minimal IPv4 header: version=4, IHL=5, total=28, src=10.99.0.1, dst=10.99.0.254
	ip := dataFromClient[protocol.HeaderSize:]
	ip[0] = 0x45
	ip[2], ip[3] = 0, 28
	copy(ip[12:16], net.ParseIP("10.99.0.1").To4())
	copy(ip[16:20], net.ParseIP("10.99.0.254").To4())
	s.handlePacket(dataFromClient, addr2)

	testData := []byte("failovertest")
	s.sendToClient(1, testData)

	// Only recv2 (the active data path) should receive.
	buf := make([]byte, 100)

	_ = recv2.SetReadDeadline(time.Now().Add(1 * time.Second))
	n, _, err := recv2.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("recv2 should receive: %v", err)
	}
	if string(buf[:n]) != "failovertest" {
		t.Errorf("recv2 got %q", buf[:n])
	}

	// recv1 should NOT receive.
	_ = recv1.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = recv1.ReadFromUDP(buf)
	if err == nil {
		t.Fatal("recv1 should not receive in failover mode")
	}
}

func TestNew_MissingServerConfig(t *testing.T) {
	cfg := &config.Config{
		Mode:    "server",
		TeamKey: "test",
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for missing server config")
	}
}

func TestServerRunContextCancel(t *testing.T) {
	cfg := testConfig()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- s.Run(ctx)
	}()

	// Give it a moment to start, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after context cancel")
	}
}

// Verify atomic seq increment.
func TestServerSeqIncrement(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	initial := atomic.LoadUint32(&s.seq)

	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	s.sendHeartbeatAck(from, net.ParseIP("10.99.0.1"), 24, protocol.AckStatusOK)
	s.sendHeartbeatAck(from, net.ParseIP("10.99.0.1"), 24, protocol.AckStatusOK)

	final := atomic.LoadUint32(&s.seq)
	if final != initial+2 {
		t.Errorf("seq = %d, want %d", final, initial+2)
	}
}

// --- Phase 2: TUN integration tests ---

func TestData_WriteToTUN_WhenDestIsServer(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	// Inject a mock TUN device.
	mock := newMockTUN("mock0")
	s.tunDev = mock

	// Register client 1.
	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 20001}
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), from)

	// Client 1 sends data to server's virtualIP (10.99.0.254).
	dataPkt := buildDataPacket(1, 500, net.ParseIP("10.99.0.1"), net.ParseIP("10.99.0.254"))
	s.handlePacket(dataPkt, from)

	// The IP payload should have been written to the mock TUN.
	select {
	case written := <-mock.writeBuf:
		// Verify the payload is a 20-byte IP header with correct addresses.
		if len(written) < 20 {
			t.Fatalf("written payload too short: %d bytes", len(written))
		}
		srcIP := net.IP(written[12:16])
		dstIP := net.IP(written[16:20])
		if !srcIP.Equal(net.ParseIP("10.99.0.1")) {
			t.Errorf("TUN write srcIP = %s, want 10.99.0.1", srcIP)
		}
		if !dstIP.Equal(net.ParseIP("10.99.0.254")) {
			t.Errorf("TUN write dstIP = %s, want 10.99.0.254", dstIP)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("expected data to be written to TUN, but timed out")
	}
}

func TestData_NoTUN_DestIsServer(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	// No TUN device set (tunDev is nil).
	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 20001}
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), from)

	// This should not panic or error; it should just log and return.
	dataPkt := buildDataPacket(1, 501, net.ParseIP("10.99.0.1"), net.ParseIP("10.99.0.254"))
	s.handlePacket(dataPkt, from)
	// If we get here without panic, the test passes.
}

func TestTunReadLoop_ForwardsToClient(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	mock := newMockTUN("mock0")
	s.tunDev = mock

	// Register client 1 with a real UDP socket to receive.
	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	from := clientConn.LocalAddr().(*net.UDPAddr)
	s.handlePacket(buildHeartbeat(1, net.ParseIP("10.99.0.1"), 24, protocol.SendModeRedundant, "testkey"), from)
	drainHeartbeatAcks(t, clientConn)

	// Start TUN read loop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tunReadLoop(ctx)

	// Inject an IP packet into the mock TUN (server -> client 10.99.0.1).
	ipPkt := make([]byte, 20)
	ipPkt[0] = 0x45
	copy(ipPkt[12:16], net.ParseIP("10.99.0.254").To4()) // src = server
	copy(ipPkt[16:20], net.ParseIP("10.99.0.1").To4())   // dst = client 1
	mock.readBuf <- ipPkt

	// Client should receive the packet.
	buf := make([]byte, 2000)
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := clientConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("client did not receive packet from TUN read loop: %v", err)
	}

	// Verify header.
	hdr, err := protocol.DecodeHeader(buf[:n])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if hdr.ClientID != 0 {
		t.Errorf("clientID = %d, want 0 (server-originated)", hdr.ClientID)
	}
	if hdr.Type != protocol.TypeData {
		t.Errorf("type = 0x%02x, want TypeData", hdr.Type)
	}

	// Verify payload matches the injected IP packet.
	payload := buf[protocol.HeaderSize:n]
	if len(payload) != 20 {
		t.Fatalf("payload length = %d, want 20", len(payload))
	}
	dstIP := net.IP(payload[16:20])
	if !dstIP.Equal(net.ParseIP("10.99.0.1")) {
		t.Errorf("payload dstIP = %s, want 10.99.0.1", dstIP)
	}
}

func TestTunReadLoop_NoRouteDrops(t *testing.T) {
	s, _, cleanup := startTestServer(t)
	defer cleanup()

	mock := newMockTUN("mock0")
	s.tunDev = mock

	// No clients registered, so no routes exist.
	ctx, cancel := context.WithCancel(context.Background())
	go s.tunReadLoop(ctx)

	// Inject an IP packet to an unknown destination.
	ipPkt := make([]byte, 20)
	ipPkt[0] = 0x45
	copy(ipPkt[12:16], net.ParseIP("10.99.0.254").To4())
	copy(ipPkt[16:20], net.ParseIP("10.99.0.99").To4()) // no such client
	mock.readBuf <- ipPkt

	// Give the loop time to process, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()
	// No crash = success; the packet should have been silently dropped.
}

// --- Phase 2: IP auto-allocation tests ---

func TestAllocateIP_AutoAssign(t *testing.T) {
	cfg := testConfig()
	cfg.Server.IPPoolFile = "" // no persistence for this test
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s.conn = conn

	// Client 1 sends heartbeat with virtualIP=0.0.0.0 (auto-assign).
	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40001}
	pkt := buildHeartbeat(1, net.IPv4zero, 0, protocol.SendModeRedundant, "testkey")
	s.handlePacket(pkt, from)

	s.sessionsLock.RLock()
	session, ok := s.sessions[1]
	if !ok {
		s.sessionsLock.RUnlock()
		t.Fatal("session not created")
	}
	assignedIP := session.VirtualIP
	s.sessionsLock.RUnlock()

	if assignedIP.Equal(net.IPv4zero) {
		t.Fatal("expected an auto-assigned IP, got 0.0.0.0")
	}

	// Verify the assigned IP is within the subnet 10.99.0.0/24.
	_, subnet, _ := net.ParseCIDR("10.99.0.0/24")
	if !subnet.Contains(assignedIP) {
		t.Errorf("assigned IP %s not in subnet %s", assignedIP, subnet)
	}

	// Verify it's not the server's own IP.
	if assignedIP.Equal(net.ParseIP("10.99.0.254")) {
		t.Error("assigned IP should not be the server's own IP")
	}

	// Verify route table entry was created.
	s.routeLock.RLock()
	var key [4]byte
	copy(key[:], assignedIP.To4())
	cid, ok := s.routeTable[key]
	s.routeLock.RUnlock()
	if !ok {
		t.Fatal("route not created for auto-assigned IP")
	}
	if cid != 1 {
		t.Errorf("route clientID = %d, want 1", cid)
	}
}

func TestAllocateIP_SameClientGetsSameIP(t *testing.T) {
	cfg := testConfig()
	cfg.Server.IPPoolFile = "" // no persistence
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s.conn = conn

	// First registration.
	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40001}
	pkt := buildHeartbeat(1, net.IPv4zero, 0, protocol.SendModeRedundant, "testkey")
	s.handlePacket(pkt, from)

	s.sessionsLock.RLock()
	firstIP := s.sessions[1].VirtualIP
	s.sessionsLock.RUnlock()

	// Simulate session timeout: remove session.
	s.sessionsLock.Lock()
	ip4 := s.sessions[1].VirtualIP.To4()
	if ip4 != nil {
		var key [4]byte
		copy(key[:], ip4)
		s.routeLock.Lock()
		delete(s.routeTable, key)
		s.routeLock.Unlock()
	}
	delete(s.sessions, 1)
	s.sessionsLock.Unlock()

	// Re-register same clientID.
	s.handlePacket(pkt, from)

	s.sessionsLock.RLock()
	secondIP := s.sessions[1].VirtualIP
	s.sessionsLock.RUnlock()

	if !firstIP.Equal(secondIP) {
		t.Errorf("reconnected client got different IP: first=%s, second=%s", firstIP, secondIP)
	}
}

func TestAllocateIP_MultipleClients(t *testing.T) {
	cfg := testConfig()
	cfg.Server.IPPoolFile = ""
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s.conn = conn

	// Register three clients with auto-assign.
	for i := uint16(1); i <= 3; i++ {
		from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40000 + int(i)}
		pkt := buildHeartbeat(i, net.IPv4zero, 0, protocol.SendModeRedundant, "testkey")
		s.handlePacket(pkt, from)
	}

	// Verify all got unique IPs.
	s.sessionsLock.RLock()
	ips := make(map[string]bool)
	for _, session := range s.sessions {
		ipStr := session.VirtualIP.String()
		if ips[ipStr] {
			t.Errorf("duplicate IP assigned: %s", ipStr)
		}
		ips[ipStr] = true
	}
	s.sessionsLock.RUnlock()

	if len(ips) != 3 {
		t.Errorf("expected 3 unique IPs, got %d", len(ips))
	}
}

func TestAllocateIP_SkipsServerIP(t *testing.T) {
	cfg := testConfig()
	cfg.Server.IPPoolFile = ""
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Allocate many IPs and verify none is the server's IP.
	for i := uint16(1); i <= 10; i++ {
		ip, _ := s.allocateIP(i)
		if ip != nil && ip.Equal(net.ParseIP("10.99.0.254")) {
			t.Fatalf("allocated server's own IP 10.99.0.254 to clientID=%d", i)
		}
	}
}

func TestIPPool_Persistence(t *testing.T) {
	tmpDir := t.TempDir()
	poolFile := filepath.Join(tmpDir, "test_ip_pool.json")

	cfg := testConfig()
	cfg.Server.IPPoolFile = poolFile
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Allocate an IP.
	ip1, prefix1 := s.allocateIP(1)
	if ip1 == nil {
		t.Fatal("allocation failed")
	}

	// Verify the file was written.
	data, err := os.ReadFile(poolFile)
	if err != nil {
		t.Fatalf("failed to read pool file: %v", err)
	}

	var entries []ipPoolEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("failed to parse pool file: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ClientID != 1 {
		t.Errorf("entry clientID = %d, want 1", entries[0].ClientID)
	}
	if entries[0].IP != ip1.String() {
		t.Errorf("entry IP = %s, want %s", entries[0].IP, ip1)
	}

	// Create a new server instance and verify it loads the pool.
	cfg2 := testConfig()
	cfg2.Server.IPPoolFile = poolFile
	s2, err := New(cfg2)
	if err != nil {
		t.Fatal(err)
	}

	ip2, prefix2 := s2.allocateIP(1)
	if !ip1.Equal(ip2) {
		t.Errorf("loaded IP = %s, want %s", ip2, ip1)
	}
	if prefix1 != prefix2 {
		t.Errorf("loaded prefix = %d, want %d", prefix2, prefix1)
	}
}
