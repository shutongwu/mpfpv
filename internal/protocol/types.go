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

// PathRTT is a single NIC name + RTT + TX/RX bytes reported by the client.
type PathRTT struct {
	Name    string
	RTTms   uint16 // milliseconds
	TxBytes uint64 // cumulative bytes sent through this path
	RxBytes uint64 // cumulative bytes received on this path
}

// HeartbeatPayload is the 16-byte heartbeat payload, optionally followed
// by a variable-length device name string and path RTT data.
type HeartbeatPayload struct {
	VirtualIP   net.IP   // 4 bytes IPv4
	PrefixLen   uint8
	SendMode    uint8
	ReplyPort   uint16   // central recv socket port; 0 = use source port (legacy)
	TeamKeyHash [8]byte
	DeviceName  string     // optional, from extended payload (beyond 16 bytes)
	PathRTTs    []PathRTT  // optional, per-NIC RTT reported by client
}

// HeartbeatAckPayload is the 8-byte heartbeat ack payload.
type HeartbeatAckPayload struct {
	AssignedIP net.IP // 4 bytes IPv4
	PrefixLen  uint8
	Status     uint8
	MTU        uint16 // TUN MTU; 0 = use client default
}
