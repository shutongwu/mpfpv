package protocol

import (
	"bytes"
	"crypto/sha256"
	"net"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{
		Version:  Version1,
		Type:     TypeData,
		Priority: false,
		ClientID: 0x1234,
		Seq:      0xDEADBEEF,
	}
	buf := make([]byte, HeaderSize)
	EncodeHeader(buf, &h)

	got, err := DecodeHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("roundtrip mismatch: got %+v, want %+v", got, h)
	}
}

func TestHeaderWithPriority(t *testing.T) {
	h := Header{
		Version:  Version1,
		Type:     TypeHeartbeat,
		Priority: true,
		ClientID: 42,
		Seq:      1,
	}
	buf := make([]byte, HeaderSize)
	EncodeHeader(buf, &h)

	got, err := DecodeHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Priority {
		t.Fatal("expected priority=true")
	}
	if got.Type != TypeHeartbeat {
		t.Fatalf("expected type heartbeat, got %d", got.Type)
	}
}

func TestDecodeHeaderVersionMismatch(t *testing.T) {
	buf := make([]byte, HeaderSize)
	buf[0] = 0x20 // version 2
	_, err := DecodeHeader(buf)
	if err != ErrVersionMismatch {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}
}

func TestDecodeHeaderBufferTooShort(t *testing.T) {
	_, err := DecodeHeader(make([]byte, 4))
	if err != ErrBufferTooShort {
		t.Fatalf("expected ErrBufferTooShort, got %v", err)
	}
}

func TestHeaderBigEndian(t *testing.T) {
	h := Header{
		Version:  Version1,
		Type:     TypeData,
		ClientID: 0x0102,
		Seq:      0x01020304,
	}
	buf := make([]byte, HeaderSize)
	EncodeHeader(buf, &h)

	// Check clientID big-endian: bytes 2,3 = 0x01, 0x02
	if buf[2] != 0x01 || buf[3] != 0x02 {
		t.Fatalf("clientID not big-endian: %x %x", buf[2], buf[3])
	}
	// Check seq big-endian: bytes 4-7 = 0x01, 0x02, 0x03, 0x04
	if buf[4] != 0x01 || buf[5] != 0x02 || buf[6] != 0x03 || buf[7] != 0x04 {
		t.Fatalf("seq not big-endian: %x", buf[4:8])
	}
}

func TestHeartbeatRoundTrip(t *testing.T) {
	hash := ComputeTeamKeyHash("my-secret-team")
	hb := HeartbeatPayload{
		VirtualIP:   net.IPv4(10, 0, 0, 1).To4(),
		PrefixLen:   24,
		SendMode:    SendModeFailover,
		TeamKeyHash: hash,
	}
	buf := make([]byte, HeartbeatPayloadSize)
	EncodeHeartbeat(buf, &hb)

	got, err := DecodeHeartbeat(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.VirtualIP.Equal(hb.VirtualIP) {
		t.Fatalf("VirtualIP mismatch: %v vs %v", got.VirtualIP, hb.VirtualIP)
	}
	if got.PrefixLen != 24 {
		t.Fatalf("PrefixLen: got %d want 24", got.PrefixLen)
	}
	if got.SendMode != SendModeFailover {
		t.Fatalf("SendMode: got %d want %d", got.SendMode, SendModeFailover)
	}
	if got.TeamKeyHash != hash {
		t.Fatal("TeamKeyHash mismatch")
	}
}

func TestHeartbeatZeroIP(t *testing.T) {
	hb := HeartbeatPayload{
		VirtualIP: net.IPv4zero.To4(),
		PrefixLen: 0,
	}
	buf := make([]byte, HeartbeatPayloadSize)
	EncodeHeartbeat(buf, &hb)

	got, err := DecodeHeartbeat(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.VirtualIP.Equal(net.IPv4zero) {
		t.Fatalf("expected 0.0.0.0, got %v", got.VirtualIP)
	}
}

func TestDecodeHeartbeatTooShort(t *testing.T) {
	_, err := DecodeHeartbeat(make([]byte, 10))
	if err != ErrBufferTooShort {
		t.Fatalf("expected ErrBufferTooShort, got %v", err)
	}
}

func TestHeartbeatAckRoundTrip(t *testing.T) {
	ack := HeartbeatAckPayload{
		AssignedIP: net.IPv4(10, 0, 0, 5).To4(),
		PrefixLen:  24,
		Status:     AckStatusOK,
	}
	buf := make([]byte, HeartbeatAckPayloadSize)
	EncodeHeartbeatAck(buf, &ack)

	got, err := DecodeHeartbeatAck(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AssignedIP.Equal(ack.AssignedIP) {
		t.Fatalf("IP mismatch: %v vs %v", got.AssignedIP, ack.AssignedIP)
	}
	if got.PrefixLen != 24 {
		t.Fatalf("PrefixLen: got %d want 24", got.PrefixLen)
	}
	if got.Status != AckStatusOK {
		t.Fatalf("Status: got %d want %d", got.Status, AckStatusOK)
	}
}

func TestHeartbeatAckStatuses(t *testing.T) {
	for _, status := range []uint8{AckStatusOK, AckStatusTeamKeyMismatch, AckStatusClientIDConflict} {
		ack := HeartbeatAckPayload{
			AssignedIP: net.IPv4(192, 168, 1, 1).To4(),
			PrefixLen:  16,
			Status:     status,
		}
		buf := make([]byte, HeartbeatAckPayloadSize)
		EncodeHeartbeatAck(buf, &ack)
		got, err := DecodeHeartbeatAck(buf)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != status {
			t.Fatalf("status %d roundtrip failed, got %d", status, got.Status)
		}
	}
}

func TestDecodeHeartbeatAckTooShort(t *testing.T) {
	_, err := DecodeHeartbeatAck(make([]byte, 5))
	if err != ErrBufferTooShort {
		t.Fatalf("expected ErrBufferTooShort, got %v", err)
	}
}

func TestComputeTeamKeyHash(t *testing.T) {
	hash := ComputeTeamKeyHash("test-key")
	sum := sha256.Sum256([]byte("test-key"))
	if !bytes.Equal(hash[:], sum[:8]) {
		t.Fatal("hash mismatch")
	}

	// Different keys produce different hashes
	hash2 := ComputeTeamKeyHash("other-key")
	if hash == hash2 {
		t.Fatal("different keys should produce different hashes")
	}
}

func TestReservedBytesZeroed(t *testing.T) {
	// Heartbeat reserved bytes 6-7 should be zero
	hb := HeartbeatPayload{
		VirtualIP: net.IPv4(10, 0, 0, 1).To4(),
		PrefixLen: 24,
		SendMode:  SendModeRedundant,
	}
	buf := make([]byte, HeartbeatPayloadSize)
	EncodeHeartbeat(buf, &hb)
	if buf[6] != 0 || buf[7] != 0 {
		t.Fatal("heartbeat reserved bytes not zeroed")
	}

	// HeartbeatAck reserved bytes 6-7 should be zero
	ack := HeartbeatAckPayload{
		AssignedIP: net.IPv4(10, 0, 0, 1).To4(),
		PrefixLen:  24,
		Status:     AckStatusOK,
	}
	abuf := make([]byte, HeartbeatAckPayloadSize)
	EncodeHeartbeatAck(abuf, &ack)
	if abuf[6] != 0 || abuf[7] != 0 {
		t.Fatal("ack reserved bytes not zeroed")
	}
}
