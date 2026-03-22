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
	buf[6] = byte(hb.ReplyPort >> 8)
	buf[7] = byte(hb.ReplyPort)
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
		ReplyPort:   uint16(buf[6])<<8 | uint16(buf[7]),
		TeamKeyHash: hash,
	}
	if len(buf) > HeartbeatPayloadSize {
		ext := buf[HeartbeatPayloadSize:]
		// Look for \x00 separator between device name and path RTT data.
		sepIdx := -1
		for i, b := range ext {
			if b == 0x00 {
				sepIdx = i
				break
			}
		}
		if sepIdx >= 0 {
			hb.DeviceName = string(ext[:sepIdx])
			// Parse path RTT data after separator.
			rttData := ext[sepIdx+1:]
			if len(rttData) >= 1 {
				count := int(rttData[0])
				pos := 1
				for i := 0; i < count && pos < len(rttData); i++ {
					nameLen := int(rttData[pos])
					pos++
					if pos+nameLen+2 > len(rttData) {
						break
					}
					name := string(rttData[pos : pos+nameLen])
					pos += nameLen
					rttMs := uint16(rttData[pos])<<8 | uint16(rttData[pos+1])
					pos += 2
					var txBytes uint64
					if pos+4 <= len(rttData) {
						tx32 := uint32(rttData[pos])<<24 | uint32(rttData[pos+1])<<16 | uint32(rttData[pos+2])<<8 | uint32(rttData[pos+3])
						txBytes = uint64(tx32)
						pos += 4
					}
					hb.PathRTTs = append(hb.PathRTTs, PathRTT{Name: name, RTTms: rttMs, TxBytes: txBytes})
				}
			}
		} else {
			hb.DeviceName = string(ext)
		}
	}
	return hb, nil
}

// EncodeHeartbeatWithName writes a heartbeat payload followed by an optional
// device name and path RTT data. buf must be large enough.
// Format: [16B fixed] [deviceName] [\x00 count nameLen name rttHi rttLo ...]
// Returns the total number of bytes written.
func EncodeHeartbeatWithName(buf []byte, hb *HeartbeatPayload, deviceName string) int {
	EncodeHeartbeat(buf, hb)
	n := HeartbeatPayloadSize
	if deviceName != "" {
		n += copy(buf[n:], []byte(deviceName))
	}
	if len(hb.PathRTTs) > 0 {
		buf[n] = 0x00 // separator
		n++
		buf[n] = byte(len(hb.PathRTTs))
		n++
		for _, p := range hb.PathRTTs {
			nameBytes := []byte(p.Name)
			buf[n] = byte(len(nameBytes))
			n++
			n += copy(buf[n:], nameBytes)
			buf[n] = byte(p.RTTms >> 8)
			buf[n+1] = byte(p.RTTms)
			n += 2
			// TxBytes as uint32 (wraps at 4GB, sufficient for rate calculation)
			tx32 := uint32(p.TxBytes)
			buf[n] = byte(tx32 >> 24)
			buf[n+1] = byte(tx32 >> 16)
			buf[n+2] = byte(tx32 >> 8)
			buf[n+3] = byte(tx32)
			n += 4
		}
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
	buf[6] = byte(ack.MTU >> 8)
	buf[7] = byte(ack.MTU)
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
		MTU:        uint16(buf[6])<<8 | uint16(buf[7]),
	}, nil
}

// ComputeTeamKeyHash returns the first 8 bytes of SHA-256(teamKey).
func ComputeTeamKeyHash(teamKey string) [8]byte {
	sum := sha256.Sum256([]byte(teamKey))
	var out [8]byte
	copy(out[:], sum[:8])
	return out
}
