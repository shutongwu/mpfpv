package protocol

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"
)

var (
	ErrBufferTooShort  = errors.New("protocol: buffer too short")
	ErrVersionMismatch = errors.New("protocol: unsupported version")
)

// EncodeHeader writes h into the first 8 bytes of buf (zero-copy).
func EncodeHeader(buf []byte, h *Header) {
	buf[0] = (h.Version & 0xF0) | (h.Type & 0x0F)
	buf[1] = 0
	if h.Priority {
		buf[1] = 0x01
	}
	binary.BigEndian.PutUint16(buf[2:4], h.ClientID)
	binary.BigEndian.PutUint32(buf[4:8], h.Seq)
}

// DecodeHeader parses an 8-byte header from buf.
func DecodeHeader(buf []byte) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, ErrBufferTooShort
	}
	version := buf[0] & 0xF0
	if version != Version1 {
		return Header{}, ErrVersionMismatch
	}
	return Header{
		Version:  version,
		Type:     buf[0] & 0x0F,
		Priority: buf[1]&0x01 != 0,
		ClientID: binary.BigEndian.Uint16(buf[2:4]),
		Seq:      binary.BigEndian.Uint32(buf[4:8]),
	}, nil
}

// EncodeHeartbeat writes hb into the first 16 bytes of buf.
func EncodeHeartbeat(buf []byte, hb *HeartbeatPayload) {
	ip := hb.VirtualIP.To4()
	if ip == nil {
		ip = net.IPv4zero.To4()
	}
	copy(buf[0:4], ip)
	buf[4] = hb.PrefixLen
	buf[5] = hb.SendMode
	buf[6] = 0
	buf[7] = 0
	copy(buf[8:16], hb.TeamKeyHash[:])
}

// DecodeHeartbeat parses a heartbeat payload from buf (at least 16 bytes).
// If buf is longer than 16 bytes, the extra bytes are interpreted as a
// UTF-8 device name string.
func DecodeHeartbeat(buf []byte) (HeartbeatPayload, error) {
	if len(buf) < HeartbeatPayloadSize {
		return HeartbeatPayload{}, ErrBufferTooShort
	}
	var hash [8]byte
	copy(hash[:], buf[8:16])
	hb := HeartbeatPayload{
		VirtualIP:   net.IP(append([]byte(nil), buf[0:4]...)),
		PrefixLen:   buf[4],
		SendMode:    buf[5],
		TeamKeyHash: hash,
	}
	if len(buf) > HeartbeatPayloadSize {
		hb.DeviceName = string(buf[HeartbeatPayloadSize:])
	}
	return hb, nil
}

// EncodeHeartbeatWithName writes a heartbeat payload followed by an optional
// device name. buf must have at least HeartbeatPayloadSize + len(deviceName)
// bytes available. Returns the total number of bytes written.
func EncodeHeartbeatWithName(buf []byte, hb *HeartbeatPayload, deviceName string) int {
	EncodeHeartbeat(buf, hb)
	n := HeartbeatPayloadSize
	if deviceName != "" {
		n += copy(buf[n:], []byte(deviceName))
	}
	return n
}

// EncodeHeartbeatAck writes ack into the first 8 bytes of buf.
func EncodeHeartbeatAck(buf []byte, ack *HeartbeatAckPayload) {
	ip := ack.AssignedIP.To4()
	if ip == nil {
		ip = net.IPv4zero.To4()
	}
	copy(buf[0:4], ip)
	buf[4] = ack.PrefixLen
	buf[5] = ack.Status
	buf[6] = 0
	buf[7] = 0
}

// DecodeHeartbeatAck parses an 8-byte heartbeat ack payload from buf.
func DecodeHeartbeatAck(buf []byte) (HeartbeatAckPayload, error) {
	if len(buf) < HeartbeatAckPayloadSize {
		return HeartbeatAckPayload{}, ErrBufferTooShort
	}
	return HeartbeatAckPayload{
		AssignedIP: net.IP(append([]byte(nil), buf[0:4]...)),
		PrefixLen:  buf[4],
		Status:     buf[5],
	}, nil
}

// ComputeTeamKeyHash returns the first 8 bytes of SHA-256(teamKey).
func ComputeTeamKeyHash(teamKey string) [8]byte {
	sum := sha256.Sum256([]byte(teamKey))
	var out [8]byte
	copy(out[:], sum[:8])
	return out
}
