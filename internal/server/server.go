package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/protocol"
	"github.com/cloud/mpfpv/internal/tunnel"
)

// AddrInfo tracks a single source address of a client.
type AddrInfo struct {
	Addr     *net.UDPAddr
	LastSeen time.Time
}

// ClientSession tracks a connected client.
type ClientSession struct {
	ClientID     uint16
	VirtualIP    net.IP
	PrefixLen    uint8
	SendMode     uint8              // protocol.SendModeRedundant or SendModeFailover
	ReplyPort    uint16             // central recv port; 0 = use source port (legacy)
	DeviceName   string             // device name reported by client
	Addrs        map[string]*AddrInfo // srcAddr string -> info
	LastSeen     time.Time
	LastDataAddr *net.UDPAddr      // last address that sent a data packet (for failover reply)
	PathRTTs     []protocol.PathRTT // per-NIC RTT reported by client
}

// Server is the mpfpv relay server.
type Server struct {
	cfg         *config.Config
	conn        *net.UDPConn
	sessions    map[uint16]*ClientSession // clientID -> session
	routeTable  map[[4]byte]uint16        // virtualIP(4bytes) -> clientID
	dedup       *protocol.Deduplicator
	teamKeyHash [8]byte
	sessionsLock sync.RWMutex
	routeLock    sync.RWMutex
	seq          uint32 // server's own seq (clientID=0), accessed atomically

	// Configurable timeouts (stored as durations for convenience).
	clientTimeout time.Duration
	addrTimeout   time.Duration

	// serverVirtualIP is the server's own virtual IP (for detecting traffic to self).
	serverVirtualIP [4]byte

	// cfgPath is the path to the configuration file (for saving changes via Web UI).
	cfgPath string

	// TUN device (optional, nil when TUN is not available).
	tunDev    tunnel.Device
	serverIP  net.IP   // server's own virtual IP as net.IP
	prefixLen uint8    // subnet prefix length

	// IP auto-allocation.
	ipPool      map[uint16]net.IP  // clientID -> allocated IP
	ipPoolNames map[uint16]string  // clientID -> device name (for persistence)
	subnet      *net.IPNet         // auto-allocation subnet
	ipPoolFile  string             // persistence file path
	ipPoolLock  sync.Mutex         // protects ipPool and ipPoolNames
}

// New creates and initializes a Server.
func New(cfg *config.Config) (*Server, error) {
	if cfg.Server == nil {
		return nil, fmt.Errorf("server: missing server configuration")
	}

	s := &Server{
		cfg:           cfg,
		sessions:      make(map[uint16]*ClientSession),
		routeTable:    make(map[[4]byte]uint16),
		dedup:         protocol.NewDeduplicator(cfg.Server.DedupWindow),
		teamKeyHash:   protocol.ComputeTeamKeyHash(cfg.TeamKey),
		clientTimeout: time.Duration(cfg.Server.ClientTimeout) * time.Second,
		addrTimeout:   time.Duration(cfg.Server.AddrTimeout) * time.Second,
		ipPool:        make(map[uint16]net.IP),
		ipPoolNames:   make(map[uint16]string),
		ipPoolFile:    cfg.Server.IPPoolFile,
	}

	// Parse server's own virtual IP.
	ip, ipNet, err := net.ParseCIDR(cfg.Server.VirtualIP)
	if err != nil {
		return nil, fmt.Errorf("server: invalid virtualIP: %w", err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("server: virtualIP must be IPv4")
	}
	copy(s.serverVirtualIP[:], ip4)
	s.serverIP = ip4

	// Determine prefix length from the CIDR mask.
	ones, _ := ipNet.Mask.Size()
	s.prefixLen = uint8(ones)

	// Parse subnet for auto-allocation.
	if cfg.Server.Subnet != "" {
		_, subnet, err := net.ParseCIDR(cfg.Server.Subnet)
		if err != nil {
			return nil, fmt.Errorf("server: invalid subnet: %w", err)
		}
		s.subnet = subnet
	}

	// Load persisted IP pool.
	s.loadIPPool()

	return s, nil
}

// Run starts the server. It blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Try to create TUN device before listening on UDP.
	s.initTUN()

	addr, err := net.ResolveUDPAddr("udp4", s.cfg.Server.ListenAddr)
	if err != nil {
		return fmt.Errorf("server: resolve listen addr: %w", err)
	}

	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("server: listen: %w", err)
	}
	s.conn = conn
	defer conn.Close()

	log.Infof("server: listening on %s", addr)

	// Start cleanup goroutine.
	go s.cleanupLoop(ctx)

	// Start TUN read loop if TUN is available.
	if s.tunDev != nil {
		go s.tunReadLoop(ctx)
	}

	// Main receive loop.
	buf := make([]byte, s.cfg.Server.MTU+protocol.HeaderSize+100)
	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown: close TUN device.
			if s.tunDev != nil {
				s.tunDev.Close()
			}
			return ctx.Err()
		default:
		}

		// Set read deadline so we can check ctx periodically.
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-ctx.Done():
				if s.tunDev != nil {
					s.tunDev.Close()
				}
				return ctx.Err()
			default:
			}
			log.Warnf("server: read error: %v", err)
			continue
		}

		s.handlePacket(buf[:n], remoteAddr)
	}
}

// initTUN attempts to create and configure the TUN device.
// If creation fails (e.g. not root, no /dev/net/tun), the server
// continues in relay-only mode without TUN.
func (s *Server) initTUN() {
	tunDev, err := tunnel.CreateTUN(tunnel.Config{
		Name:      "mpfpv0",
		MTU:       s.cfg.Server.MTU,
		VirtualIP: s.serverIP,
		PrefixLen: int(s.prefixLen),
	})
	if err != nil {
		log.Warnf("server: TUN creation failed, running in relay-only mode: %v", err)
		return
	}
	s.tunDev = tunDev
	log.Infof("server: TUN device %s created with IP %s/%d", tunDev.Name(), s.serverIP, s.prefixLen)
}

// tunReadLoop reads IP packets from the TUN device and forwards them
// to the appropriate client based on the destination IP in the packet.
func (s *Server) tunReadLoop(ctx context.Context) {
	buf := make([]byte, s.cfg.Server.MTU+protocol.HeaderSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := s.tunDev.Read(buf[protocol.HeaderSize:])
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Warnf("server: TUN read error: %v (continuing)", err)
			continue
		}

		if n < 20 {
			continue // too short for an IPv4 header
		}

		// Only forward IPv4 packets (version nibble == 4).
		if buf[protocol.HeaderSize]>>4 != 4 {
			continue
		}

		payload := buf[protocol.HeaderSize : protocol.HeaderSize+n]

		// Read destination IP from the IPv4 header (bytes 16-19).
		var dstIP [4]byte
		copy(dstIP[:], payload[16:20])

		// Look up the destination client in the route table.
		s.routeLock.RLock()
		clientID, ok := s.routeTable[dstIP]
		s.routeLock.RUnlock()
		if !ok {
			continue // destination not in route table, drop
		}

		// Encode header with clientID=0 (server-originated traffic).
		seq := atomic.AddUint32(&s.seq, 1) - 1
		protocol.EncodeHeader(buf, &protocol.Header{
			Version:  protocol.Version1,
			Type:     protocol.TypeData,
			ClientID: 0,
			Seq:      seq,
		})

		// Send to the destination client.
		s.sendToClient(clientID, buf[:protocol.HeaderSize+n])
	}
}

// handlePacket processes a single incoming UDP packet.
func (s *Server) handlePacket(data []byte, from *net.UDPAddr) {
	if len(data) < protocol.HeaderSize {
		log.Debugf("server: packet too short from %s", from)
		return
	}

	hdr, err := protocol.DecodeHeader(data)
	if err != nil {
		log.Debugf("server: decode header from %s: %v", from, err)
		return
	}

	payload := data[protocol.HeaderSize:]

	switch hdr.Type {
	case protocol.TypeHeartbeat:
		s.handleHeartbeat(hdr, payload, from)
	case protocol.TypeData:
		s.handleData(hdr, payload, from, data)
	default:
		log.Debugf("server: unknown message type 0x%02x from %s", hdr.Type, from)
	}
}

// handleHeartbeat processes a Heartbeat message.
func (s *Server) handleHeartbeat(hdr protocol.Header, payload []byte, from *net.UDPAddr) {
	hb, err := protocol.DecodeHeartbeat(payload)
	if err != nil {
		log.Warnf("server: decode heartbeat from %s: %v", from, err)
		return
	}

	// Check teamKey hash.
	if hb.TeamKeyHash != s.teamKeyHash {
		log.Warnf("server: teamKey mismatch from clientID=%d addr=%s", hdr.ClientID, from)
		s.sendHeartbeatAck(from, net.IPv4zero, 0, protocol.AckStatusTeamKeyMismatch)
		return
	}

	clientID := hdr.ClientID
	if clientID == 0 {
		log.Warnf("server: received heartbeat with reserved clientID=0 from %s", from)
		return
	}

	vip := hb.VirtualIP.To4()
	if vip == nil {
		vip = net.IPv4zero.To4()
	}
	prefixLen := hb.PrefixLen

	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()

	// Check clientID conflict: same clientID but different virtualIP.
	if existing, ok := s.sessions[clientID]; ok {
		if !vip.Equal(net.IPv4zero) && !existing.VirtualIP.Equal(vip) {
			log.Warnf("server: clientID=%d conflict: registered IP=%s, heartbeat IP=%s from %s",
				clientID, existing.VirtualIP, vip, from)
			s.sendHeartbeatAck(from, existing.VirtualIP, existing.PrefixLen, protocol.AckStatusClientIDConflict)
			return
		}
	}

	now := time.Now()
	session, exists := s.sessions[clientID]
	if !exists {
		// New client.
		assignedIP := vip
		assignedPrefix := prefixLen

		// If virtualIP is 0.0.0.0, auto-assign from pool.
		if assignedIP.Equal(net.IPv4zero) {
			allocated, allocPrefix := s.allocateIP(clientID, hb.DeviceName)
			if allocated != nil {
				assignedIP = allocated
				assignedPrefix = allocPrefix
				log.Infof("server: auto-assigned IP %s/%d to clientID=%d", assignedIP, assignedPrefix, clientID)
			} else {
				log.Warnf("server: clientID=%d requested auto-assign IP, but allocation failed", clientID)
			}
		}

		session = &ClientSession{
			ClientID:   clientID,
			VirtualIP:  assignedIP,
			PrefixLen:  assignedPrefix,
			SendMode:   hb.SendMode,
			ReplyPort:  hb.ReplyPort,
			DeviceName: hb.DeviceName,
			PathRTTs:   hb.PathRTTs,
			Addrs:      make(map[string]*AddrInfo),
			LastSeen:   now,
		}
		s.sessions[clientID] = session

		// Update route table.
		if !assignedIP.Equal(net.IPv4zero) {
			var key [4]byte
			copy(key[:], assignedIP.To4())
			s.routeLock.Lock()
			s.routeTable[key] = clientID
			s.routeLock.Unlock()
		}

		log.Infof("server: new client registered: clientID=%d virtualIP=%s sendMode=%d deviceName=%q from %s",
			clientID, assignedIP, hb.SendMode, hb.DeviceName, from)
	} else {
		// Update existing session.
		session.SendMode = hb.SendMode
		session.ReplyPort = hb.ReplyPort
		session.LastSeen = now
		if hb.DeviceName != "" {
			session.DeviceName = hb.DeviceName
		}
		if len(hb.PathRTTs) > 0 {
			session.PathRTTs = hb.PathRTTs
		}

		// If client reconnects with 0.0.0.0, use the previously assigned IP.
		if vip.Equal(net.IPv4zero) && !session.VirtualIP.Equal(net.IPv4zero) {
			// Client is requesting auto-assign but already has an IP; keep it.
		}
	}

	// Update/add source address.
	addrKey := from.String()
	if ai, ok := session.Addrs[addrKey]; ok {
		ai.LastSeen = now
	} else {
		session.Addrs[addrKey] = &AddrInfo{
			Addr:     from,
			LastSeen: now,
		}
		log.Infof("server: clientID=%d added addr %s (total: %d)", clientID, addrKey, len(session.Addrs))
	}

	s.sendHeartbeatAck(from, session.VirtualIP, session.PrefixLen, protocol.AckStatusOK)
}

// handleData processes a Data message.
func (s *Server) handleData(hdr protocol.Header, payload []byte, from *net.UDPAddr, rawPacket []byte) {
	clientID := hdr.ClientID

	// Dedup.
	if s.dedup.IsDuplicate(clientID, hdr.Seq) {
		log.Debugf("server: dedup dropped packet from clientID=%d seq=%d addr=%s", clientID, hdr.Seq, from)
		return
	}
	log.Debugf("server: data packet from clientID=%d seq=%d len=%d addr=%s", clientID, hdr.Seq, len(rawPacket), from)

	// Update addr LastSeen (but don't learn routes from Data).
	s.sessionsLock.RLock()
	session, exists := s.sessions[clientID]
	if !exists {
		s.sessionsLock.RUnlock()
		log.Debugf("server: data from unknown clientID=%d addr=%s, dropping", clientID, from)
		return
	}

	// Validate inner IP: must be IPv4, at least 20 bytes.
	if len(payload) < 20 || payload[0]>>4 != 4 {
		s.sessionsLock.RUnlock()
		log.Debugf("server: invalid inner IP from clientID=%d len=%d ver=%d", clientID, len(payload), payload[0]>>4)
		return
	}

	// IPv4 header: src IP at bytes 12-15, dst IP at bytes 16-19.
	var srcIP [4]byte
	copy(srcIP[:], payload[12:16])
	var registeredIP [4]byte
	regIP4 := session.VirtualIP.To4()
	if regIP4 != nil {
		copy(registeredIP[:], regIP4)
	}

	if srcIP != registeredIP {
		s.sessionsLock.RUnlock()
		log.Warnf("server: inner IP src %d.%d.%d.%d != registered %s for clientID=%d, dropping",
			srcIP[0], srcIP[1], srcIP[2], srcIP[3], session.VirtualIP, clientID)
		return
	}

	// Update source address last seen time.
	addrKey := from.String()
	if ai, ok := session.Addrs[addrKey]; ok {
		ai.LastSeen = time.Now()
	}
	session.LastSeen = time.Now()
	session.LastDataAddr = from
	s.sessionsLock.RUnlock()

	// Read destination IP from inner IP header.
	var dstIP [4]byte
	copy(dstIP[:], payload[16:20])

	// Check if destination is server itself — write to TUN.
	if dstIP == s.serverVirtualIP {
		if s.tunDev != nil {
			pkt := make([]byte, len(payload))
			copy(pkt, payload)
			if _, err := s.tunDev.Write(pkt); err != nil {
				log.Warnf("server: TUN write error: %v", err)
			}
		} else {
			log.Debugf("server: data for server virtualIP from clientID=%d (TUN not available)", clientID)
		}
		return
	}

	// Look up destination client.
	s.routeLock.RLock()
	dstClientID, found := s.routeTable[dstIP]
	s.routeLock.RUnlock()

	if !found {
		log.Debugf("server: no route for dst %d.%d.%d.%d from clientID=%d",
			dstIP[0], dstIP[1], dstIP[2], dstIP[3], clientID)
		return
	}

	// Forward to destination client.
	s.sendToClient(dstClientID, rawPacket)
}

// sendToClient sends a raw packet to the specified client according to its sendMode.
func (s *Server) sendToClient(clientID uint16, rawPacket []byte) {
	s.sessionsLock.RLock()
	session, exists := s.sessions[clientID]
	if !exists {
		s.sessionsLock.RUnlock()
		return
	}

	sendMode := session.SendMode
	replyPort := session.ReplyPort
	lastData := session.LastDataAddr
	addrs := make([]*AddrInfo, 0, len(session.Addrs))
	for _, ai := range session.Addrs {
		addrs = append(addrs, ai)
	}
	s.sessionsLock.RUnlock()

	if len(addrs) == 0 {
		return
	}

	// sendTo is a helper that sends to the address and optionally also to
	// the ReplyPort variant. The original port goes through NAT; the
	// ReplyPort targets the NIC-independent central recv socket (local
	// network or when NAT is not involved).
	sendTo := func(dst *net.UDPAddr) {
		if _, err := s.conn.WriteToUDP(rawPacket, dst); err != nil {
			log.Warnf("server: write to %s for clientID=%d: %v", dst, clientID, err)
		}
		// Also send to ReplyPort if it differs from the original port.
		if replyPort > 0 && int(replyPort) != dst.Port {
			rpAddr := &net.UDPAddr{IP: dst.IP, Port: int(replyPort), Zone: dst.Zone}
			s.conn.WriteToUDP(rawPacket, rpAddr) // best-effort, ignore error
		}
	}

	switch sendMode {
	case protocol.SendModeRedundant:
		// Send to all known addresses.
		for _, ai := range addrs {
			sendTo(ai.Addr)
		}
	case protocol.SendModeFailover:
		// Send to the address that last sent a data packet.
		// This tracks the client's actual active path, not heartbeat activity.
		if lastData != nil {
			sendTo(lastData)
		} else if len(addrs) > 0 {
			sendTo(addrs[0].Addr)
		}
	default:
		// Default to redundant.
		for _, ai := range addrs {
			sendTo(ai.Addr)
		}
	}
}

// sendHeartbeatAck sends a HeartbeatAck to the given address.
func (s *Server) sendHeartbeatAck(to *net.UDPAddr, assignedIP net.IP, prefixLen uint8, status uint8) {
	buf := make([]byte, protocol.HeaderSize+protocol.HeartbeatAckPayloadSize)

	seq := atomic.AddUint32(&s.seq, 1)
	protocol.EncodeHeader(buf, &protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeHeartbeatAck,
		ClientID: 0, // server uses clientID=0
		Seq:      seq,
	})

	protocol.EncodeHeartbeatAck(buf[protocol.HeaderSize:], &protocol.HeartbeatAckPayload{
		AssignedIP: assignedIP,
		PrefixLen:  prefixLen,
		Status:     status,
		MTU:        uint16(s.cfg.Server.MTU),
	})

	if s.conn != nil {
		if _, err := s.conn.WriteToUDP(buf, to); err != nil {
			log.Warnf("server: send heartbeat ack to %s: %v", to, err)
		}
	}
}

// --- IP auto-allocation ---

// ipPoolEntry is the JSON-serializable form of an IP pool entry.
type ipPoolEntry struct {
	ClientID uint16 `json:"clientID"`
	IP       string `json:"ip"`
	Name     string `json:"name,omitempty"`
}

// allocateIP assigns a virtual IP to the given clientID from the subnet pool.
// If the clientID already has an allocation, it returns the existing one.
// The deviceName is stored alongside the allocation for persistence.
// Returns nil if allocation is not possible (no subnet configured, pool exhausted).
func (s *Server) allocateIP(clientID uint16, deviceName ...string) (net.IP, uint8) {
	s.ipPoolLock.Lock()
	defer s.ipPoolLock.Unlock()

	// Update device name if provided.
	if len(deviceName) > 0 && deviceName[0] != "" {
		s.ipPoolNames[clientID] = deviceName[0]
	}

	// Check if already allocated.
	if ip, ok := s.ipPool[clientID]; ok {
		// Re-save to persist updated device name.
		if len(deviceName) > 0 && deviceName[0] != "" {
			s.saveIPPool()
		}
		return ip, s.prefixLen
	}

	if s.subnet == nil {
		return nil, 0
	}

	// Build a set of already-used IPs for fast lookup.
	used := make(map[[4]byte]bool)
	for _, ip := range s.ipPool {
		var key [4]byte
		copy(key[:], ip.To4())
		used[key] = true
	}
	// Also mark server's own IP as used.
	used[s.serverVirtualIP] = true

	// Iterate through the subnet to find the next available IP.
	// Skip network address (first) and broadcast address (last).
	networkIP := s.subnet.IP.To4()
	mask := s.subnet.Mask
	ones, bits := mask.Size()
	hostBits := uint(bits - ones)

	// For a /24 subnet, we have 254 usable addresses (1..254).
	// For a /16, up to 65534, etc.
	maxHosts := (uint32(1) << hostBits) - 1 // broadcast offset

	baseIP := binary.BigEndian.Uint32(networkIP)

	for offset := uint32(1); offset < maxHosts; offset++ {
		candidateUint := baseIP + offset
		var candidate [4]byte
		binary.BigEndian.PutUint32(candidate[:], candidateUint)

		if used[candidate] {
			continue
		}

		allocatedIP := net.IP(make([]byte, 4))
		copy(allocatedIP, candidate[:])
		s.ipPool[clientID] = allocatedIP
		s.saveIPPool()
		return allocatedIP, s.prefixLen
	}

	return nil, 0 // pool exhausted
}

// loadIPPool loads the IP pool from the persistence file.
func (s *Server) loadIPPool() {
	if s.ipPoolFile == "" {
		return
	}

	data, err := os.ReadFile(s.ipPoolFile)
	if err != nil {
		if os.IsNotExist(err) {
			return // no file yet, start with empty pool
		}
		log.Warnf("server: failed to load IP pool from %s: %v", s.ipPoolFile, err)
		return
	}

	var entries []ipPoolEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Warnf("server: failed to parse IP pool from %s: %v", s.ipPoolFile, err)
		return
	}

	s.ipPoolLock.Lock()
	defer s.ipPoolLock.Unlock()

	for _, e := range entries {
		ip := net.ParseIP(e.IP).To4()
		if ip != nil {
			s.ipPool[e.ClientID] = ip
			if e.Name != "" {
				s.ipPoolNames[e.ClientID] = e.Name
			}
		}
	}

	log.Infof("server: loaded %d IP pool entries from %s", len(entries), s.ipPoolFile)
}

// saveIPPool persists the current IP pool to the file.
// Must be called with ipPoolLock held.
func (s *Server) saveIPPool() {
	if s.ipPoolFile == "" {
		return
	}

	entries := make([]ipPoolEntry, 0, len(s.ipPool))
	for cid, ip := range s.ipPool {
		entries = append(entries, ipPoolEntry{
			ClientID: cid,
			IP:       ip.String(),
			Name:     s.ipPoolNames[cid],
		})
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Warnf("server: failed to marshal IP pool: %v", err)
		return
	}

	if err := os.WriteFile(s.ipPoolFile, data, 0644); err != nil {
		log.Warnf("server: failed to save IP pool to %s: %v", s.ipPoolFile, err)
	}
}

// cleanupLoop periodically removes timed-out addresses and sessions.
func (s *Server) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanup()
		}
	}
}

// cleanup removes expired addresses and sessions.
func (s *Server) cleanup() {
	now := time.Now()

	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()

	for clientID, session := range s.sessions {
		// Remove expired addresses.
		for addrKey, ai := range session.Addrs {
			if now.Sub(ai.LastSeen) > s.addrTimeout {
				delete(session.Addrs, addrKey)
				log.Infof("server: clientID=%d addr %s timed out", clientID, addrKey)
			}
		}

		// If all addresses expired and session itself is timed out, remove session.
		if len(session.Addrs) == 0 && now.Sub(session.LastSeen) > s.clientTimeout {
			// Remove from route table.
			ip4 := session.VirtualIP.To4()
			if ip4 != nil {
				var key [4]byte
				copy(key[:], ip4)
				s.routeLock.Lock()
				delete(s.routeTable, key)
				s.routeLock.Unlock()
			}
			delete(s.sessions, clientID)
			s.dedup.Reset(clientID)
			log.Infof("server: clientID=%d session timed out, removed", clientID)
		}
	}
}
