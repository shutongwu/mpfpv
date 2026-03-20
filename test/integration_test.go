//go:build integration

package integration_test

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/protocol"
	"github.com/cloud/mpfpv/internal/server"
)

// ---- helpers ----------------------------------------------------------------

// startServer creates a server with the given teamKey, binds to a random
// localhost port, and starts it in a background goroutine. It returns the
// server instance, the actual listen address, and a cancel function.
func startServer(t *testing.T, teamKey string, clientTimeout, addrTimeout int) (net.Addr, context.CancelFunc) {
	t.Helper()

	// Bind to :0 to get a random port.
	tmpConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	listenAddr := tmpConn.LocalAddr().String()
	tmpConn.Close()

	cfg := &config.Config{
		Mode:    "server",
		TeamKey: teamKey,
		Server: &config.ServerConfig{
			ListenAddr:    listenAddr,
			VirtualIP:     "10.99.0.254/24",
			Subnet:        "10.99.0.0/24",
			ClientTimeout: clientTimeout,
			AddrTimeout:   addrTimeout,
			DedupWindow:   4096,
			MTU:           1300,
			IPPoolFile:    "ip_pool.json",
		},
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = srv.Run(ctx)
	}()

	// Give the server a moment to start listening.
	time.Sleep(50 * time.Millisecond)

	addr, _ := net.ResolveUDPAddr("udp4", listenAddr)
	return addr, cancel
}

// buildHeartbeat constructs a full heartbeat packet (header + payload).
func buildHeartbeat(clientID uint16, seq uint32, virtualIP net.IP, prefixLen uint8, sendMode uint8, teamKey string) []byte {
	buf := make([]byte, protocol.HeaderSize+protocol.HeartbeatPayloadSize)
	protocol.EncodeHeader(buf, &protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeHeartbeat,
		ClientID: clientID,
		Seq:      seq,
	})
	protocol.EncodeHeartbeat(buf[protocol.HeaderSize:], &protocol.HeartbeatPayload{
		VirtualIP:   virtualIP,
		PrefixLen:   prefixLen,
		SendMode:    sendMode,
		TeamKeyHash: protocol.ComputeTeamKeyHash(teamKey),
	})
	return buf
}

// buildDataPacket constructs a full data packet. The inner payload is a
// minimal 20-byte IPv4 header (just enough for the server to read src/dst IP).
func buildDataPacket(clientID uint16, seq uint32, srcIP, dstIP net.IP) []byte {
	innerIP := make([]byte, 20)
	innerIP[0] = 0x45 // version=4, IHL=5
	binary.BigEndian.PutUint16(innerIP[2:4], 20)
	innerIP[8] = 64  // TTL
	innerIP[9] = 0   // protocol (placeholder)
	copy(innerIP[12:16], srcIP.To4())
	copy(innerIP[16:20], dstIP.To4())

	buf := make([]byte, protocol.HeaderSize+len(innerIP))
	protocol.EncodeHeader(buf, &protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeData,
		ClientID: clientID,
		Seq:      seq,
	})
	copy(buf[protocol.HeaderSize:], innerIP)
	return buf
}

// readPacket reads a UDP packet with a timeout and returns the header and raw
// payload. Returns an error on timeout.
func readPacket(conn *net.UDPConn, timeout time.Duration) (protocol.Header, []byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 65535)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return protocol.Header{}, nil, err
	}
	hdr, err := protocol.DecodeHeader(buf[:n])
	if err != nil {
		return protocol.Header{}, nil, err
	}
	payload := make([]byte, n-protocol.HeaderSize)
	copy(payload, buf[protocol.HeaderSize:n])
	return hdr, payload, nil
}

// ---- tests ------------------------------------------------------------------

const teamKey = "test-team-secret"

func TestHeartbeatRegistrationAndAck(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	// Open a raw UDP socket acting as a client.
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	clientIP := net.ParseIP("10.99.0.1").To4()
	hb := buildHeartbeat(1, 0, clientIP, 24, protocol.SendModeRedundant, teamKey)

	_, err = conn.WriteToUDP(hb, srvAddr.(*net.UDPAddr))
	if err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}

	// Read the ack.
	hdr, payload, err := readPacket(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}

	if hdr.Type != protocol.TypeHeartbeatAck {
		t.Fatalf("expected HeartbeatAck, got type 0x%02x", hdr.Type)
	}

	ack, err := protocol.DecodeHeartbeatAck(payload)
	if err != nil {
		t.Fatalf("decode ack: %v", err)
	}

	if ack.Status != protocol.AckStatusOK {
		t.Fatalf("expected AckStatusOK (0x00), got 0x%02x", ack.Status)
	}
	if !ack.AssignedIP.Equal(clientIP) {
		t.Fatalf("expected assigned IP %s, got %s", clientIP, ack.AssignedIP)
	}
	if ack.PrefixLen != 24 {
		t.Fatalf("expected prefix 24, got %d", ack.PrefixLen)
	}
}

func TestTeamKeyMismatch(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	// Send heartbeat with wrong teamKey.
	hb := buildHeartbeat(1, 0, net.ParseIP("10.99.0.1").To4(), 24, protocol.SendModeRedundant, "wrong-key")

	_, err = conn.WriteToUDP(hb, srvAddr.(*net.UDPAddr))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	hdr, payload, err := readPacket(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}

	if hdr.Type != protocol.TypeHeartbeatAck {
		t.Fatalf("expected HeartbeatAck, got type 0x%02x", hdr.Type)
	}

	ack, err := protocol.DecodeHeartbeatAck(payload)
	if err != nil {
		t.Fatalf("decode ack: %v", err)
	}

	if ack.Status != protocol.AckStatusTeamKeyMismatch {
		t.Fatalf("expected AckStatusTeamKeyMismatch (0x01), got 0x%02x", ack.Status)
	}
}

func TestBasicForwarding(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	udpSrvAddr := srvAddr.(*net.UDPAddr)

	// Client A: clientID=1, virtualIP=10.99.0.1
	connA, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	defer connA.Close()

	// Client B: clientID=2, virtualIP=10.99.0.2
	connB, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	defer connB.Close()

	ipA := net.ParseIP("10.99.0.1").To4()
	ipB := net.ParseIP("10.99.0.2").To4()

	// Register both clients.
	_, err = connA.WriteToUDP(buildHeartbeat(1, 0, ipA, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	if err != nil {
		t.Fatalf("send hb A: %v", err)
	}
	_, err = connB.WriteToUDP(buildHeartbeat(2, 0, ipB, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	if err != nil {
		t.Fatalf("send hb B: %v", err)
	}

	// Read and discard heartbeat acks.
	if _, _, err := readPacket(connA, 2*time.Second); err != nil {
		t.Fatalf("ack A: %v", err)
	}
	if _, _, err := readPacket(connB, 2*time.Second); err != nil {
		t.Fatalf("ack B: %v", err)
	}

	// Client A sends data to client B.
	dataPkt := buildDataPacket(1, 1, ipA, ipB)
	_, err = connA.WriteToUDP(dataPkt, udpSrvAddr)
	if err != nil {
		t.Fatalf("send data: %v", err)
	}

	// Client B should receive the forwarded packet.
	hdr, payload, err := readPacket(connB, 2*time.Second)
	if err != nil {
		t.Fatalf("client B did not receive forwarded packet: %v", err)
	}

	if hdr.Type != protocol.TypeData {
		t.Fatalf("expected Data type, got 0x%02x", hdr.Type)
	}
	if hdr.ClientID != 1 {
		t.Fatalf("expected clientID=1 in forwarded packet, got %d", hdr.ClientID)
	}

	// Verify inner IP addresses.
	if len(payload) < 20 {
		t.Fatalf("forwarded payload too short: %d bytes", len(payload))
	}
	var fwdSrc, fwdDst [4]byte
	copy(fwdSrc[:], payload[12:16])
	copy(fwdDst[:], payload[16:20])

	if !net.IP(fwdSrc[:]).Equal(ipA) {
		t.Fatalf("forwarded inner srcIP mismatch: got %v", net.IP(fwdSrc[:]))
	}
	if !net.IP(fwdDst[:]).Equal(ipB) {
		t.Fatalf("forwarded inner dstIP mismatch: got %v", net.IP(fwdDst[:]))
	}

	// Client A should NOT receive the packet (it's not the destination).
	_ = connA.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	tmp := make([]byte, 1024)
	n, _, readErr := connA.ReadFromUDP(tmp)
	if readErr == nil && n > 0 {
		t.Fatalf("client A unexpectedly received %d bytes", n)
	}
}

func TestDeduplication(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	udpSrvAddr := srvAddr.(*net.UDPAddr)

	connA, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	defer connA.Close()

	connB, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	defer connB.Close()

	ipA := net.ParseIP("10.99.0.1").To4()
	ipB := net.ParseIP("10.99.0.2").To4()

	// Register both.
	_, _ = connA.WriteToUDP(buildHeartbeat(1, 0, ipA, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _ = connB.WriteToUDP(buildHeartbeat(2, 0, ipB, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(connA, 2*time.Second)
	_, _, _ = readPacket(connB, 2*time.Second)

	// Client A sends the SAME data packet (same seq) twice.
	dataPkt := buildDataPacket(1, 1, ipA, ipB)
	_, _ = connA.WriteToUDP(dataPkt, udpSrvAddr)
	// Small delay to ensure first packet is processed before the duplicate.
	time.Sleep(50 * time.Millisecond)
	_, _ = connA.WriteToUDP(dataPkt, udpSrvAddr)

	// Client B should receive exactly ONE packet.
	_, _, err = readPacket(connB, 2*time.Second)
	if err != nil {
		t.Fatalf("client B should receive the first packet: %v", err)
	}

	// Second read should timeout — the duplicate was dropped.
	_ = connB.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	tmp := make([]byte, 1024)
	n, _, readErr := connB.ReadFromUDP(tmp)
	if readErr == nil && n > 0 {
		t.Fatalf("client B received a duplicate packet (%d bytes) — dedup failed", n)
	}
}

func TestSessionTimeoutCleanup(t *testing.T) {
	// Use very short timeouts: clientTimeout=2s, addrTimeout=1s.
	srvAddr, cancel := startServer(t, teamKey, 2, 1)
	defer cancel()

	udpSrvAddr := srvAddr.(*net.UDPAddr)

	// Client A registers, then stops heartbeating.
	connA, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	defer connA.Close()

	// Client B is the destination — registers and stays alive.
	connB, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	defer connB.Close()

	ipA := net.ParseIP("10.99.0.1").To4()
	ipB := net.ParseIP("10.99.0.2").To4()

	// Register client A.
	_, _ = connA.WriteToUDP(buildHeartbeat(1, 0, ipA, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(connA, 2*time.Second)

	// Register client B.
	_, _ = connB.WriteToUDP(buildHeartbeat(2, 0, ipB, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(connB, 2*time.Second)

	// Verify forwarding works initially: B sends data to A.
	dataPkt := buildDataPacket(2, 1, ipB, ipA)
	_, _ = connB.WriteToUDP(dataPkt, udpSrvAddr)
	_, _, err = readPacket(connA, 2*time.Second)
	if err != nil {
		t.Fatalf("initial forwarding to A should work: %v", err)
	}

	// Now wait for client A's session to expire.
	// addrTimeout=1s, clientTimeout=2s. Cleanup runs every 1s.
	// After addrTimeout, addrs are removed; then after clientTimeout, session is removed.
	// Wait long enough for cleanup to fire and remove the session.
	time.Sleep(4 * time.Second)

	// Keep client B alive with a fresh heartbeat.
	_, _ = connB.WriteToUDP(buildHeartbeat(2, 1, ipB, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(connB, 2*time.Second)

	// B sends data to A again — should be dropped because A's session is gone.
	dataPkt2 := buildDataPacket(2, 2, ipB, ipA)
	_, _ = connB.WriteToUDP(dataPkt2, udpSrvAddr)

	// Client A should NOT receive anything.
	_ = connA.SetReadDeadline(time.Now().Add(1 * time.Second))
	tmp := make([]byte, 1024)
	n, _, readErr := connA.ReadFromUDP(tmp)
	if readErr == nil && n > 0 {
		t.Fatalf("client A received data after session should have expired (%d bytes)", n)
	}

	// Verify A can re-register.
	_, _ = connA.WriteToUDP(buildHeartbeat(1, 100, ipA, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	hdr, payload, err := readPacket(connA, 2*time.Second)
	if err != nil {
		t.Fatalf("re-registration ack failed: %v", err)
	}
	if hdr.Type != protocol.TypeHeartbeatAck {
		t.Fatalf("expected HeartbeatAck on re-register, got 0x%02x", hdr.Type)
	}
	ack, _ := protocol.DecodeHeartbeatAck(payload)
	if ack.Status != protocol.AckStatusOK {
		t.Fatalf("expected OK on re-register, got 0x%02x", ack.Status)
	}
}
