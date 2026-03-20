package protocol

import "net"

// Header size in bytes.
const HeaderSize = 8

// Protocol version (upper 4 bits of Flags byte).
const Version1 = 0x10

// Message types (lower 4 bits of Flags byte).
const (
	TypeData         = 0x00
	TypeHeartbeat    = 0x01
	TypeHeartbeatAck = 0x02
)

// Send modes for HeartbeatPayload.
const (
	SendModeRedundant = 0x00
	SendModeFailover  = 0x01
)

// HeartbeatAck status codes.
const (
	AckStatusOK              = 0x00
	AckStatusTeamKeyMismatch = 0x01
	AckStatusClientIDConflict = 0x02
)

// Payload sizes.
const (
	HeartbeatPayloadSize    = 16
	HeartbeatAckPayloadSize = 8
)

// Default deduplication window size.
const DefaultWindowSize = 4096

// Header is the 8-byte UDP encapsulation header.
type Header struct {
	Version  uint8  // upper 4 bits of Flags
	Type     uint8  // lower 4 bits of Flags
	Priority bool   // lowest bit of Reserved byte
	ClientID uint16 // big-endian
	Seq      uint32 // big-endian, per-clientID
}

// HeartbeatPayload is the 16-byte heartbeat payload.
type HeartbeatPayload struct {
	VirtualIP   net.IP   // 4 bytes IPv4
	PrefixLen   uint8
	SendMode    uint8
	TeamKeyHash [8]byte
}

// HeartbeatAckPayload is the 8-byte heartbeat ack payload.
type HeartbeatAckPayload struct {
	AssignedIP net.IP // 4 bytes IPv4
	PrefixLen  uint8
	Status     uint8
}
