//go:build integration

package integration_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
			MTU:           1400,
			IPPoolFile:    filepath.Join(os.TempDir(), fmt.Sprintf("mpfpv_test_pool_%d.json", time.Now().UnixNano())),
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

// ---- Phase 2 tests ---------------------------------------------------------

// TestAutoIPAllocation verifies that a client sending virtualIP=0.0.0.0
// receives a non-zero assigned IP from the server's subnet pool.
func TestAutoIPAllocation(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	// Send heartbeat with virtualIP = 0.0.0.0 (request auto-assignment).
	hb := buildHeartbeat(10, 0, net.IPv4zero.To4(), 0, protocol.SendModeRedundant, teamKey)
	_, err = conn.WriteToUDP(hb, srvAddr.(*net.UDPAddr))
	if err != nil {
		t.Fatalf("send heartbeat: %v", err)
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
	if ack.Status != protocol.AckStatusOK {
		t.Fatalf("expected AckStatusOK, got 0x%02x", ack.Status)
	}

	// Assigned IP must not be 0.0.0.0.
	if ack.AssignedIP.Equal(net.IPv4zero) {
		t.Fatal("server returned 0.0.0.0 — auto-allocation failed")
	}

	// Must be within the 10.99.0.0/24 subnet.
	_, subnet, _ := net.ParseCIDR("10.99.0.0/24")
	if !subnet.Contains(ack.AssignedIP) {
		t.Fatalf("assigned IP %s is not in subnet %s", ack.AssignedIP, subnet)
	}

	// Prefix length must match the server's configured /24.
	if ack.PrefixLen != 24 {
		t.Fatalf("expected prefix 24, got %d", ack.PrefixLen)
	}
}

// TestAutoIPReconnectSameIP verifies that the same clientID gets back the
// same auto-assigned IP after its session expires and it re-registers.
func TestAutoIPReconnectSameIP(t *testing.T) {
	// Short timeouts so the session expires quickly.
	srvAddr, cancel := startServer(t, teamKey, 2, 1)
	defer cancel()

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	udpAddr := srvAddr.(*net.UDPAddr)

	// First registration: auto-assign.
	hb := buildHeartbeat(20, 0, net.IPv4zero.To4(), 0, protocol.SendModeRedundant, teamKey)
	_, err = conn.WriteToUDP(hb, udpAddr)
	if err != nil {
		t.Fatalf("send hb1: %v", err)
	}

	_, payload1, err := readPacket(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read ack1: %v", err)
	}
	ack1, _ := protocol.DecodeHeartbeatAck(payload1)
	if ack1.Status != protocol.AckStatusOK {
		t.Fatalf("ack1 status: 0x%02x", ack1.Status)
	}
	firstIP := ack1.AssignedIP

	// Wait for the session to fully expire.
	time.Sleep(4 * time.Second)

	// Re-register with the same clientID and virtualIP=0.0.0.0.
	hb2 := buildHeartbeat(20, 100, net.IPv4zero.To4(), 0, protocol.SendModeRedundant, teamKey)
	_, err = conn.WriteToUDP(hb2, udpAddr)
	if err != nil {
		t.Fatalf("send hb2: %v", err)
	}

	_, payload2, err := readPacket(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read ack2: %v", err)
	}
	ack2, _ := protocol.DecodeHeartbeatAck(payload2)
	if ack2.Status != protocol.AckStatusOK {
		t.Fatalf("ack2 status: 0x%02x", ack2.Status)
	}

	if !ack2.AssignedIP.Equal(firstIP) {
		t.Fatalf("reconnect got different IP: first=%s, second=%s", firstIP, ack2.AssignedIP)
	}
}

// TestAutoIPMultiClientNoConflict verifies that 3 clients auto-assigned IPs
// all receive distinct addresses.
func TestAutoIPMultiClientNoConflict(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	udpAddr := srvAddr.(*net.UDPAddr)

	type result struct {
		clientID uint16
		ip       net.IP
	}

	results := make([]result, 3)
	for i := 0; i < 3; i++ {
		conn, err := net.ListenUDP("udp4", nil)
		if err != nil {
			t.Fatalf("listen client %d: %v", i, err)
		}
		defer conn.Close()

		cid := uint16(30 + i)
		hb := buildHeartbeat(cid, 0, net.IPv4zero.To4(), 0, protocol.SendModeRedundant, teamKey)
		_, err = conn.WriteToUDP(hb, udpAddr)
		if err != nil {
			t.Fatalf("send hb client %d: %v", i, err)
		}

		_, payload, err := readPacket(conn, 2*time.Second)
		if err != nil {
			t.Fatalf("read ack client %d: %v", i, err)
		}
		ack, _ := protocol.DecodeHeartbeatAck(payload)
		if ack.Status != protocol.AckStatusOK {
			t.Fatalf("client %d ack status: 0x%02x", i, ack.Status)
		}
		if ack.AssignedIP.Equal(net.IPv4zero) {
			t.Fatalf("client %d got 0.0.0.0", i)
		}
		results[i] = result{clientID: cid, ip: ack.AssignedIP}
	}

	// Verify all IPs are distinct.
	seen := make(map[string]uint16)
	for _, r := range results {
		key := r.ip.String()
		if prev, ok := seen[key]; ok {
			t.Fatalf("IP conflict: clientID=%d and clientID=%d both got %s", prev, r.clientID, key)
		}
		seen[key] = r.clientID
	}

	// Also verify none of the IPs is the server's own virtual IP (10.99.0.254).
	serverIP := net.ParseIP("10.99.0.254").To4()
	for _, r := range results {
		if r.ip.Equal(serverIP) {
			t.Fatalf("clientID=%d was assigned the server's own IP %s", r.clientID, serverIP)
		}
	}
}

// TestDataToServerVirtualIP verifies that a data packet whose inner
// destination is the server's own virtual IP is NOT forwarded to any client.
// The server should either write it to TUN (if available) or silently drop it.
func TestDataToServerVirtualIP(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	udpAddr := srvAddr.(*net.UDPAddr)

	// Register client A (clientID=1) and client B (clientID=2).
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
	serverIP := net.ParseIP("10.99.0.254").To4()

	// Register both clients with static IPs.
	_, _ = connA.WriteToUDP(buildHeartbeat(1, 0, ipA, 24, protocol.SendModeRedundant, teamKey), udpAddr)
	_, _ = connB.WriteToUDP(buildHeartbeat(2, 0, ipB, 24, protocol.SendModeRedundant, teamKey), udpAddr)
	_, _, _ = readPacket(connA, 2*time.Second)
	_, _, _ = readPacket(connB, 2*time.Second)

	// Client A sends data to the SERVER's virtual IP (10.99.0.254).
	dataPkt := buildDataPacket(1, 1, ipA, serverIP)
	_, err = connA.WriteToUDP(dataPkt, udpAddr)
	if err != nil {
		t.Fatalf("send data to server VIP: %v", err)
	}

	// Neither client A nor client B should receive the packet.
	// The server should handle it locally (TUN write or silent drop).
	_ = connA.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	tmp := make([]byte, 1024)
	n, _, readErr := connA.ReadFromUDP(tmp)
	if readErr == nil && n > 0 {
		// Could be a late heartbeat ack; check if it's a Data packet.
		if n >= protocol.HeaderSize {
			hdr, _ := protocol.DecodeHeader(tmp[:n])
			if hdr.Type == protocol.TypeData {
				t.Fatalf("client A received data packet that was destined to server VIP")
			}
		}
	}

	_ = connB.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	n, _, readErr = connB.ReadFromUDP(tmp)
	if readErr == nil && n > 0 {
		if n >= protocol.HeaderSize {
			hdr, _ := protocol.DecodeHeader(tmp[:n])
			if hdr.Type == protocol.TypeData {
				t.Fatalf("client B received data packet that was destined to server VIP")
			}
		}
	}
}

// ---- Phase 3 tests: Multi-NIC redundancy ---------------------------------

// TestMultiAddrRegistration verifies that one clientID sending heartbeats
// from two different UDP sockets (simulating two NICs) results in two
// source addresses being registered in the server session.
func TestMultiAddrRegistration(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	udpSrvAddr := srvAddr.(*net.UDPAddr)

	// Two sockets simulating two NICs for the same clientID=1.
	conn1, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen conn1: %v", err)
	}
	defer conn1.Close()

	conn2, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen conn2: %v", err)
	}
	defer conn2.Close()

	ipA := net.ParseIP("10.99.0.1").To4()

	// Send heartbeat from socket 1.
	hb1 := buildHeartbeat(1, 0, ipA, 24, protocol.SendModeRedundant, teamKey)
	_, err = conn1.WriteToUDP(hb1, udpSrvAddr)
	if err != nil {
		t.Fatalf("send hb from conn1: %v", err)
	}
	_, _, err = readPacket(conn1, 2*time.Second)
	if err != nil {
		t.Fatalf("ack from conn1: %v", err)
	}

	// Send heartbeat from socket 2 (same clientID, different source port).
	hb2 := buildHeartbeat(1, 1, ipA, 24, protocol.SendModeRedundant, teamKey)
	_, err = conn2.WriteToUDP(hb2, udpSrvAddr)
	if err != nil {
		t.Fatalf("send hb from conn2: %v", err)
	}
	_, _, err = readPacket(conn2, 2*time.Second)
	if err != nil {
		t.Fatalf("ack from conn2: %v", err)
	}

	// Now verify that a data packet sent to clientID=1 arrives on BOTH sockets.
	// Register a sender (clientID=2) and send data to clientID=1's virtualIP.
	connSender, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen sender: %v", err)
	}
	defer connSender.Close()

	ipSender := net.ParseIP("10.99.0.2").To4()
	_, _ = connSender.WriteToUDP(buildHeartbeat(2, 0, ipSender, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(connSender, 2*time.Second)

	dataPkt := buildDataPacket(2, 1, ipSender, ipA)
	_, err = connSender.WriteToUDP(dataPkt, udpSrvAddr)
	if err != nil {
		t.Fatalf("send data: %v", err)
	}

	// Both conn1 and conn2 should receive the forwarded data (redundant mode).
	// This implicitly verifies that 2 addresses are registered.
	_, _, err1 := readPacket(conn1, 2*time.Second)
	_, _, err2 := readPacket(conn2, 2*time.Second)

	if err1 != nil {
		t.Fatalf("conn1 did not receive data — addr not registered: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("conn2 did not receive data — second addr not registered: %v", err2)
	}
}

// TestRedundantSendMode verifies that in redundant mode, data sent to a
// clientID with two registered addresses is delivered to BOTH addresses.
func TestRedundantSendMode(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	udpSrvAddr := srvAddr.(*net.UDPAddr)

	// ClientID=1 registers from two sockets with sendMode=redundant.
	conn1, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen conn1: %v", err)
	}
	defer conn1.Close()

	conn2, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen conn2: %v", err)
	}
	defer conn2.Close()

	ipA := net.ParseIP("10.99.0.1").To4()

	_, _ = conn1.WriteToUDP(buildHeartbeat(1, 0, ipA, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(conn1, 2*time.Second)
	_, _ = conn2.WriteToUDP(buildHeartbeat(1, 1, ipA, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(conn2, 2*time.Second)

	// ClientID=2 sends data to clientID=1.
	connB, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	defer connB.Close()

	ipB := net.ParseIP("10.99.0.2").To4()
	_, _ = connB.WriteToUDP(buildHeartbeat(2, 0, ipB, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(connB, 2*time.Second)

	// Send 3 data packets.
	for seq := uint32(1); seq <= 3; seq++ {
		dataPkt := buildDataPacket(2, seq, ipB, ipA)
		_, err = connB.WriteToUDP(dataPkt, udpSrvAddr)
		if err != nil {
			t.Fatalf("send data seq=%d: %v", seq, err)
		}
		// Small delay to ensure ordering.
		time.Sleep(20 * time.Millisecond)
	}

	// Both sockets should receive all 3 packets.
	for seq := uint32(1); seq <= 3; seq++ {
		_, _, err1 := readPacket(conn1, 2*time.Second)
		if err1 != nil {
			t.Fatalf("conn1 did not receive packet seq=%d: %v", seq, err1)
		}
		_, _, err2 := readPacket(conn2, 2*time.Second)
		if err2 != nil {
			t.Fatalf("conn2 did not receive packet seq=%d: %v", seq, err2)
		}
	}
}

// TestFailoverSendMode verifies that in failover mode, data sent to a
// clientID with two registered addresses is delivered to only ONE address
// (the most recently active one).
func TestFailoverSendMode(t *testing.T) {
	srvAddr, cancel := startServer(t, teamKey, 15, 5)
	defer cancel()

	udpSrvAddr := srvAddr.(*net.UDPAddr)

	// ClientID=1 registers from two sockets with sendMode=failover.
	conn1, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen conn1: %v", err)
	}
	defer conn1.Close()

	conn2, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen conn2: %v", err)
	}
	defer conn2.Close()

	ipA := net.ParseIP("10.99.0.1").To4()

	// Register addr1 first.
	_, _ = conn1.WriteToUDP(buildHeartbeat(1, 0, ipA, 24, protocol.SendModeFailover, teamKey), udpSrvAddr)
	_, _, _ = readPacket(conn1, 2*time.Second)

	// Then register addr2 (this will be the most recently seen).
	time.Sleep(50 * time.Millisecond)
	_, _ = conn2.WriteToUDP(buildHeartbeat(1, 1, ipA, 24, protocol.SendModeFailover, teamKey), udpSrvAddr)
	_, _, _ = readPacket(conn2, 2*time.Second)

	// ClientID=2 sends data to clientID=1.
	connB, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	defer connB.Close()

	ipB := net.ParseIP("10.99.0.2").To4()
	_, _ = connB.WriteToUDP(buildHeartbeat(2, 0, ipB, 24, protocol.SendModeFailover, teamKey), udpSrvAddr)
	_, _, _ = readPacket(connB, 2*time.Second)

	dataPkt := buildDataPacket(2, 1, ipB, ipA)
	_, err = connB.WriteToUDP(dataPkt, udpSrvAddr)
	if err != nil {
		t.Fatalf("send data: %v", err)
	}

	// Only conn2 (most recently active) should receive data.
	// We read from both with a short timeout to check.
	received1 := 0
	received2 := 0

	_ = conn1.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf1 := make([]byte, 65535)
	n1, _, err1 := conn1.ReadFromUDP(buf1)
	if err1 == nil && n1 > 0 {
		received1++
	}

	_ = conn2.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf2 := make([]byte, 65535)
	n2, _, err2 := conn2.ReadFromUDP(buf2)
	if err2 == nil && n2 > 0 {
		received2++
	}

	total := received1 + received2
	if total != 1 {
		t.Fatalf("failover: expected exactly 1 socket to receive data, got %d (conn1=%d, conn2=%d)",
			total, received1, received2)
	}
	if received2 != 1 {
		t.Fatalf("failover: expected conn2 (most recent) to receive data, but conn1 received it instead")
	}
}

// TestAddrTimeoutPartial verifies that when clientID=1 has two addresses and
// only one keeps heartbeating, the stale address is removed but the session
// survives (because one address is still active).
func TestAddrTimeoutPartial(t *testing.T) {
	// addrTimeout=2s, clientTimeout=10s.
	srvAddr, cancel := startServer(t, teamKey, 10, 2)
	defer cancel()

	udpSrvAddr := srvAddr.(*net.UDPAddr)

	// ClientID=1 registers from two sockets.
	conn1, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen conn1: %v", err)
	}
	defer conn1.Close()

	conn2, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen conn2: %v", err)
	}
	defer conn2.Close()

	ipA := net.ParseIP("10.99.0.1").To4()

	// Register both addresses.
	_, _ = conn1.WriteToUDP(buildHeartbeat(1, 0, ipA, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(conn1, 2*time.Second)
	_, _ = conn2.WriteToUDP(buildHeartbeat(1, 1, ipA, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(conn2, 2*time.Second)

	// Keep only conn1 alive; let conn2's address expire.
	// Send heartbeats from conn1 every second for 4 seconds (> addrTimeout=2s).
	for i := 0; i < 4; i++ {
		time.Sleep(1 * time.Second)
		_, _ = conn1.WriteToUDP(buildHeartbeat(1, uint32(10+i), ipA, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
		_, _, _ = readPacket(conn1, 2*time.Second)
	}

	// Now conn2's address should have been cleaned up, but the session remains.
	// Register clientID=2 as a sender.
	connSender, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen sender: %v", err)
	}
	defer connSender.Close()

	ipSender := net.ParseIP("10.99.0.2").To4()
	_, _ = connSender.WriteToUDP(buildHeartbeat(2, 0, ipSender, 24, protocol.SendModeRedundant, teamKey), udpSrvAddr)
	_, _, _ = readPacket(connSender, 2*time.Second)

	// Send data to clientID=1.
	dataPkt := buildDataPacket(2, 1, ipSender, ipA)
	_, err = connSender.WriteToUDP(dataPkt, udpSrvAddr)
	if err != nil {
		t.Fatalf("send data: %v", err)
	}

	// conn1 (still alive) should receive the data.
	_, _, err = readPacket(conn1, 2*time.Second)
	if err != nil {
		t.Fatalf("conn1 should still receive data (session alive): %v", err)
	}

	// conn2 (expired) should NOT receive data.
	_ = conn2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	tmp := make([]byte, 1024)
	n, _, readErr := conn2.ReadFromUDP(tmp)
	if readErr == nil && n > 0 {
		t.Fatalf("conn2 received data despite its address being expired (%d bytes)", n)
	}
}

// TestIPv6_HeartbeatRoundtrip verifies that a server listening on [::1] can
// handle heartbeats from an IPv6 client and reply with HeartbeatAck.
func TestIPv6_HeartbeatRoundtrip(t *testing.T) {
	teamKey := "ipv6-test"

	// Start server on IPv6 loopback.
	tmpConn, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 not available, skipping")
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
			ClientTimeout: 15,
			AddrTimeout:   5,
			DedupWindow:   4096,
			MTU:           1400,
			IPPoolFile:    filepath.Join(os.TempDir(), fmt.Sprintf("mpfpv_ipv6_pool_%d.json", time.Now().UnixNano())),
		},
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	// Create IPv6 client socket.
	srvAddr, _ := net.ResolveUDPAddr("udp6", listenAddr)
	clientConn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback, Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer clientConn.Close()

	// Send heartbeat.
	hb := buildHeartbeat(100, 0, net.IPv4zero, 24, protocol.SendModeRedundant, teamKey)
	if _, err := clientConn.WriteToUDP(hb, srvAddr); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}

	// Read HeartbeatAck.
	hdr, _, err := readPacket(clientConn, 2*time.Second)
	if err != nil {
		t.Fatalf("no heartbeat ack received over IPv6: %v", err)
	}
	if hdr.Type != protocol.TypeHeartbeatAck {
		t.Fatalf("expected HeartbeatAck, got type %d", hdr.Type)
	}
	t.Logf("IPv6 heartbeat roundtrip OK: server=%s", listenAddr)
}
