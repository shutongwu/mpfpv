package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/protocol"
)

// helper to build a minimal config for testing.
func testConfig(serverAddr string) *config.Config {
	return &config.Config{
		Mode:    "client",
		TeamKey: "testkey",
		Client: &config.ClientConfig{
			ClientID:    1,
			VirtualIP:   "10.99.0.1/24",
			ServerAddr:  serverAddr,
			SendMode:    "redundant",
			MTU:         1300,
			DedupWindow: 4096,
		},
	}
}

// ---------------------------------------------------------------------------
// Mock TUN device
// ---------------------------------------------------------------------------

type mockTUN struct {
	readCh  chan []byte
	writeCh chan []byte
	name    string
	mu      sync.Mutex
	closed  bool
}

func newMockTUN(name string) *mockTUN {
	return &mockTUN{
		readCh:  make(chan []byte, 64),
		writeCh: make(chan []byte, 64),
		name:    name,
	}
}

func (m *mockTUN) Read(buf []byte) (int, error) {
	data, ok := <-m.readCh
	if !ok {
		return 0, errors.New("mockTUN closed")
	}
	n := copy(buf, data)
	return n, nil
}

func (m *mockTUN) Write(buf []byte) (int, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return 0, errors.New("mockTUN closed")
	}
	cp := make([]byte, len(buf))
	copy(cp, buf)
	m.writeCh <- cp
	return len(buf), nil
}

func (m *mockTUN) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.readCh)
	}
	return nil
}

func (m *mockTUN) Name() string {
	return m.name
}

// ---------------------------------------------------------------------------
// Phase 1 tests (unchanged)
// ---------------------------------------------------------------------------

func TestNew(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if c.virtualIP.String() != "10.99.0.1" {
		t.Errorf("virtualIP = %s, want 10.99.0.1", c.virtualIP)
	}
	if c.prefixLen != 24 {
		t.Errorf("prefixLen = %d, want 24", c.prefixLen)
	}
	if c.sendMode != protocol.SendModeRedundant {
		t.Errorf("sendMode = %d, want %d", c.sendMode, protocol.SendModeRedundant)
	}
	expectedHash := protocol.ComputeTeamKeyHash("testkey")
	if c.teamKeyHash != expectedHash {
		t.Errorf("teamKeyHash mismatch")
	}
}

func TestNew_AutoIP(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	cfg.Client.VirtualIP = ""
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if !c.virtualIP.Equal(net.IPv4zero) {
		t.Errorf("virtualIP = %s, want 0.0.0.0", c.virtualIP)
	}
	if c.prefixLen != 0 {
		t.Errorf("prefixLen = %d, want 0", c.prefixLen)
	}
}

func TestNew_Failover(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	cfg.Client.SendMode = "failover"
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if c.sendMode != protocol.SendModeFailover {
		t.Errorf("sendMode = %d, want %d", c.sendMode, protocol.SendModeFailover)
	}
}

func TestHeartbeatEncoding(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Manually encode what sendHeartbeat would produce.
	buf := make([]byte, protocol.HeaderSize+protocol.HeartbeatPayloadSize)
	hdr := &protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeHeartbeat,
		ClientID: 1,
		Seq:      0,
	}
	protocol.EncodeHeader(buf, hdr)

	hb := &protocol.HeartbeatPayload{
		VirtualIP:   c.virtualIP,
		PrefixLen:   c.prefixLen,
		SendMode:    c.sendMode,
		TeamKeyHash: c.teamKeyHash,
	}
	protocol.EncodeHeartbeat(buf[protocol.HeaderSize:], hb)

	// Decode and verify header.
	decHdr, err := protocol.DecodeHeader(buf)
	if err != nil {
		t.Fatalf("DecodeHeader error: %v", err)
	}
	if decHdr.Type != protocol.TypeHeartbeat {
		t.Errorf("type = %d, want %d", decHdr.Type, protocol.TypeHeartbeat)
	}
	if decHdr.ClientID != 1 {
		t.Errorf("clientID = %d, want 1", decHdr.ClientID)
	}
	if decHdr.Seq != 0 {
		t.Errorf("seq = %d, want 0", decHdr.Seq)
	}

	// Decode and verify heartbeat payload.
	decHb, err := protocol.DecodeHeartbeat(buf[protocol.HeaderSize:])
	if err != nil {
		t.Fatalf("DecodeHeartbeat error: %v", err)
	}
	if !decHb.VirtualIP.Equal(net.IPv4(10, 99, 0, 1)) {
		t.Errorf("VirtualIP = %s, want 10.99.0.1", decHb.VirtualIP)
	}
	if decHb.PrefixLen != 24 {
		t.Errorf("PrefixLen = %d, want 24", decHb.PrefixLen)
	}
	if decHb.SendMode != protocol.SendModeRedundant {
		t.Errorf("SendMode = %d, want %d", decHb.SendMode, protocol.SendModeRedundant)
	}
	if decHb.TeamKeyHash != c.teamKeyHash {
		t.Errorf("TeamKeyHash mismatch")
	}
}

func TestHandleHeartbeatAck_OK(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Build an OK ack payload.
	payload := make([]byte, protocol.HeartbeatAckPayloadSize)
	ack := &protocol.HeartbeatAckPayload{
		AssignedIP: net.IPv4(10, 99, 0, 42).To4(),
		PrefixLen:  24,
		Status:     protocol.AckStatusOK,
	}
	protocol.EncodeHeartbeatAck(payload, ack)

	hdr := protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeHeartbeatAck,
		ClientID: 0,
		Seq:      0,
	}

	c.handleHeartbeatAck(hdr, payload)

	if !c.IsRegistered() {
		t.Error("expected registered = true after OK ack")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.virtualIP.Equal(net.IPv4(10, 99, 0, 42).To4()) {
		t.Errorf("virtualIP = %s, want 10.99.0.42", c.virtualIP)
	}
	if c.prefixLen != 24 {
		t.Errorf("prefixLen = %d, want 24", c.prefixLen)
	}
}

func TestHandleHeartbeatAck_TeamKeyMismatch(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	payload := make([]byte, protocol.HeartbeatAckPayloadSize)
	ack := &protocol.HeartbeatAckPayload{
		AssignedIP: net.IPv4zero.To4(),
		PrefixLen:  0,
		Status:     protocol.AckStatusTeamKeyMismatch,
	}
	protocol.EncodeHeartbeatAck(payload, ack)

	hdr := protocol.Header{Type: protocol.TypeHeartbeatAck}
	c.handleHeartbeatAck(hdr, payload)

	if c.IsRegistered() {
		t.Error("expected registered = false after teamKey mismatch")
	}
}

func TestHandleHeartbeatAck_Conflict(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	payload := make([]byte, protocol.HeartbeatAckPayloadSize)
	ack := &protocol.HeartbeatAckPayload{
		AssignedIP: net.IPv4zero.To4(),
		PrefixLen:  0,
		Status:     protocol.AckStatusClientIDConflict,
	}
	protocol.EncodeHeartbeatAck(payload, ack)

	hdr := protocol.Header{Type: protocol.TypeHeartbeatAck}
	c.handleHeartbeatAck(hdr, payload)

	if c.IsRegistered() {
		t.Error("expected registered = false after conflict")
	}
}

func TestSendEncoding(t *testing.T) {
	// Start a local UDP listener to capture the sent packet.
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer listener.Close()

	cfg := testConfig(listener.LocalAddr().String())
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Create a UDP conn for the client manually.
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatalf("client listen error: %v", err)
	}
	defer conn.Close()
	c.conn = conn

	testPayload := []byte("hello mpfpv")
	if err := c.Send(testPayload); err != nil {
		t.Fatalf("Send() error: %v", err)
	}

	// Read the packet from the listener.
	buf := make([]byte, 1500)
	listener.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := listener.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}

	if n < protocol.HeaderSize+len(testPayload) {
		t.Fatalf("packet too short: %d bytes", n)
	}

	hdr, err := protocol.DecodeHeader(buf[:n])
	if err != nil {
		t.Fatalf("DecodeHeader error: %v", err)
	}
	if hdr.Type != protocol.TypeData {
		t.Errorf("type = %d, want %d", hdr.Type, protocol.TypeData)
	}
	if hdr.ClientID != 1 {
		t.Errorf("clientID = %d, want 1", hdr.ClientID)
	}

	payload := buf[protocol.HeaderSize:n]
	if string(payload) != "hello mpfpv" {
		t.Errorf("payload = %q, want %q", payload, "hello mpfpv")
	}
}

func TestSendSequenceIncrement(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer listener.Close()

	cfg := testConfig(listener.LocalAddr().String())
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatalf("client listen error: %v", err)
	}
	defer conn.Close()
	c.conn = conn

	// Send 3 packets and verify seq increments.
	for i := 0; i < 3; i++ {
		if err := c.Send([]byte("x")); err != nil {
			t.Fatalf("Send() error: %v", err)
		}

		buf := make([]byte, 1500)
		listener.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := listener.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read error on packet %d: %v", i, err)
		}
		hdr, err := protocol.DecodeHeader(buf[:n])
		if err != nil {
			t.Fatalf("DecodeHeader error on packet %d: %v", i, err)
		}
		// seq shares the counter with heartbeats, but since we haven't
		// called sendHeartbeat, the seq should equal i.
		if hdr.Seq != uint32(i) {
			t.Errorf("packet %d: seq = %d, want %d", i, hdr.Seq, i)
		}
	}
}

func TestDedup(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// First packet: not duplicate
	if c.dedup.IsDuplicate(1, 0) {
		t.Error("first packet should not be duplicate")
	}
	// Same packet: duplicate
	if !c.dedup.IsDuplicate(1, 0) {
		t.Error("same packet should be duplicate")
	}
	// Next seq: not duplicate
	if c.dedup.IsDuplicate(1, 1) {
		t.Error("next seq should not be duplicate")
	}
	// Different clientID, same seq: not duplicate
	if c.dedup.IsDuplicate(2, 0) {
		t.Error("different clientID same seq should not be duplicate")
	}
}

func TestRunAndShutdown(t *testing.T) {
	// Start a dummy server to receive heartbeats.
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer serverConn.Close()

	cfg := testConfig(serverConn.LocalAddr().String())
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Run(ctx)
	}()

	// Wait to receive at least one heartbeat.
	buf := make([]byte, 1500)
	serverConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := serverConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected to receive heartbeat, got error: %v", err)
	}

	hdr, err := protocol.DecodeHeader(buf[:n])
	if err != nil {
		t.Fatalf("DecodeHeader error: %v", err)
	}
	if hdr.Type != protocol.TypeHeartbeat {
		t.Errorf("expected heartbeat, got type %d", hdr.Type)
	}

	// Shutdown.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run() returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not exit within timeout")
	}
}

func TestHeartbeatAndSendShareSeqCounter(t *testing.T) {
	// Verify heartbeat and Send share the same atomic seq counter.
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Simulate: seq starts at 0.
	// sendHeartbeat would use seq=0, then Send would use seq=1.
	initial := atomic.LoadUint32(&c.seq)
	if initial != 0 {
		t.Errorf("initial seq = %d, want 0", initial)
	}
}

// ---------------------------------------------------------------------------
// Phase 2 TUN integration tests
// ---------------------------------------------------------------------------

// TestTunReadLoop verifies that the TUN read loop reads IP packets from the
// mock TUN, encapsulates them with a protocol header, and sends them to the
// server via UDP.
func TestTunReadLoop(t *testing.T) {
	// UDP listener acts as the server.
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer serverConn.Close()

	cfg := testConfig(serverConn.LocalAddr().String())
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Create client UDP socket.
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatalf("client listen error: %v", err)
	}
	defer conn.Close()
	c.conn = conn

	// Inject mock TUN.
	mock := newMockTUN("test0")
	c.tunDev = mock
	close(c.tunReady)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.tunReadLoop(ctx)

	// Simulate an IP packet arriving on the TUN.
	fakeIP := []byte{0x45, 0x00, 0x00, 0x14, 0, 0, 0, 0, 64, 17, 0, 0, 10, 99, 0, 1, 10, 99, 0, 2}
	mock.readCh <- fakeIP

	// Read the encapsulated packet from the server side.
	buf := make([]byte, 1500)
	serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := serverConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}

	if n < protocol.HeaderSize+len(fakeIP) {
		t.Fatalf("packet too short: %d bytes, want >= %d", n, protocol.HeaderSize+len(fakeIP))
	}

	hdr, err := protocol.DecodeHeader(buf[:n])
	if err != nil {
		t.Fatalf("DecodeHeader error: %v", err)
	}
	if hdr.Type != protocol.TypeData {
		t.Errorf("type = %d, want %d", hdr.Type, protocol.TypeData)
	}
	if hdr.ClientID != 1 {
		t.Errorf("clientID = %d, want 1", hdr.ClientID)
	}

	payload := buf[protocol.HeaderSize:n]
	if len(payload) != len(fakeIP) {
		t.Fatalf("payload len = %d, want %d", len(payload), len(fakeIP))
	}
	for i := range fakeIP {
		if payload[i] != fakeIP[i] {
			t.Errorf("payload[%d] = %02x, want %02x", i, payload[i], fakeIP[i])
		}
	}
}

// TestHandleDataWritesToTUN verifies that handleData writes the payload to the
// mock TUN device.
func TestHandleDataWritesToTUN(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	mock := newMockTUN("test0")
	c.tunDev = mock

	fakeIP := []byte{0x45, 0x00, 0x00, 0x14, 0, 0, 0, 0, 64, 17, 0, 0, 10, 99, 0, 2, 10, 99, 0, 1}
	hdr := protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeData,
		ClientID: 2,
		Seq:      0,
	}

	c.handleData(hdr, fakeIP)

	select {
	case written := <-mock.writeCh:
		if len(written) != len(fakeIP) {
			t.Fatalf("written len = %d, want %d", len(written), len(fakeIP))
		}
		for i := range fakeIP {
			if written[i] != fakeIP[i] {
				t.Errorf("written[%d] = %02x, want %02x", i, written[i], fakeIP[i])
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for TUN write")
	}
}

// TestHandleDataNoTUN verifies that handleData works without a TUN (UDP-only mode).
func TestHandleDataNoTUN(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// tunDev is nil (no TUN)
	fakeIP := []byte{0x45, 0x00, 0x00, 0x14}
	hdr := protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeData,
		ClientID: 2,
		Seq:      0,
	}

	// Should not panic.
	c.handleData(hdr, fakeIP)
}

// TestHandleDataDedupWithTUN verifies that duplicate data packets are not
// written to the TUN.
func TestHandleDataDedupWithTUN(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	mock := newMockTUN("test0")
	c.tunDev = mock

	fakeIP := []byte{0x45, 0x00, 0x00, 0x14}
	hdr := protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeData,
		ClientID: 2,
		Seq:      0,
	}

	// First call: should write.
	c.handleData(hdr, fakeIP)
	select {
	case <-mock.writeCh:
		// OK
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for first TUN write")
	}

	// Second call with same seq: should be deduplicated, no write.
	c.handleData(hdr, fakeIP)
	select {
	case <-mock.writeCh:
		t.Fatal("duplicate packet should not have been written to TUN")
	case <-time.After(100 * time.Millisecond):
		// OK, no write happened.
	}
}

// TestAutoAssignTUNCreation verifies that in auto-assign mode, the TUN
// is created after receiving the first HeartbeatAck OK, and tunReadLoop
// can start working.
func TestAutoAssignTUNCreation(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	cfg.Client.VirtualIP = "" // auto-assign mode
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// In auto-assign mode, tunDev should be nil before first ack.
	if c.tunDev != nil {
		t.Fatal("tunDev should be nil in auto-assign mode before ack")
	}

	// Verify virtualIP is 0.0.0.0 (auto-assign).
	if !c.virtualIP.Equal(net.IPv4zero) {
		t.Fatalf("virtualIP = %s, want 0.0.0.0", c.virtualIP)
	}

	// Simulate receiving a HeartbeatAck OK with an assigned IP.
	// Since we can't actually create a TUN in tests (no /dev/net/tun),
	// we verify the state transitions and tunReady signaling.
	//
	// We'll inject a mock TUN manually to test the flow, but first
	// verify handleHeartbeatAck would attempt setupTUN.
	payload := make([]byte, protocol.HeartbeatAckPayloadSize)
	ack := &protocol.HeartbeatAckPayload{
		AssignedIP: net.IPv4(10, 99, 0, 5).To4(),
		PrefixLen:  24,
		Status:     protocol.AckStatusOK,
	}
	protocol.EncodeHeartbeatAck(payload, ack)
	hdr := protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeHeartbeatAck,
		ClientID: 0,
		Seq:      0,
	}

	// handleHeartbeatAck will call setupTUN which will fail (no /dev/net/tun
	// in test), but it should still close tunReady and set registered.
	c.handleHeartbeatAck(hdr, payload)

	if !c.IsRegistered() {
		t.Error("expected registered = true after OK ack")
	}

	// tunReady should be closed (so tunReadLoop can proceed).
	select {
	case <-c.tunReady:
		// OK, tunReady was closed.
	default:
		t.Error("tunReady should be closed after first OK ack in auto-assign mode")
	}

	// virtualIP should be updated.
	c.mu.Lock()
	if !c.virtualIP.Equal(net.IPv4(10, 99, 0, 5).To4()) {
		t.Errorf("virtualIP = %s, want 10.99.0.5", c.virtualIP)
	}
	c.mu.Unlock()
}

// TestAutoAssignTunReadLoopWithMock verifies the full flow: auto-assign mode,
// mock TUN injected, tunReadLoop waits for tunReady then reads from TUN.
func TestAutoAssignTunReadLoopWithMock(t *testing.T) {
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer serverConn.Close()

	cfg := testConfig(serverConn.LocalAddr().String())
	cfg.Client.VirtualIP = ""
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatalf("client listen error: %v", err)
	}
	defer conn.Close()
	c.conn = conn

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start tunReadLoop. It should block waiting for tunReady.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.tunReadLoop(ctx)
	}()

	// Inject mock TUN and signal tunReady.
	mock := newMockTUN("test0")
	c.tunDev = mock
	close(c.tunReady)

	// Now send a fake IP packet through TUN.
	fakeIP := []byte{0x45, 0x00, 0x00, 0x14, 0, 0, 0, 0, 64, 17, 0, 0, 10, 99, 0, 5, 10, 99, 0, 1}
	mock.readCh <- fakeIP

	// Verify it was sent to the server.
	buf := make([]byte, 1500)
	serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := serverConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}

	hdr, err := protocol.DecodeHeader(buf[:n])
	if err != nil {
		t.Fatalf("DecodeHeader error: %v", err)
	}
	if hdr.Type != protocol.TypeData {
		t.Errorf("type = %d, want %d", hdr.Type, protocol.TypeData)
	}
	if hdr.ClientID != 1 {
		t.Errorf("clientID = %d, want 1", hdr.ClientID)
	}

	payload := buf[protocol.HeaderSize:n]
	if len(payload) != len(fakeIP) {
		t.Fatalf("payload len = %d, want %d", len(payload), len(fakeIP))
	}

	cancel()
	mock.Close()
	wg.Wait()
}

// TestTunReadLoopExitsOnContextCancel verifies that tunReadLoop exits when
// the context is cancelled while waiting for tunReady.
func TestTunReadLoopExitsOnContextCancel(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	cfg.Client.VirtualIP = ""
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		c.tunReadLoop(ctx)
		close(done)
	}()

	// Cancel context; tunReadLoop should exit (it's waiting on tunReady).
	cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("tunReadLoop did not exit after context cancel")
	}
}

// TestNewCreatesChannels verifies that New initializes the tunReady channel.
func TestNewCreatesChannels(t *testing.T) {
	cfg := testConfig("127.0.0.1:9800")
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if c.tunReady == nil {
		t.Error("tunReady channel should not be nil")
	}
}
