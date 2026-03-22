package server

import (
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/protocol"
	"github.com/cloud/mpfpv/internal/web"
)

// SetConfigPath sets the path to the configuration file, enabling
// the server to save configuration changes made via the Web UI.
func (s *Server) SetConfigPath(path string) {
	s.cfgPath = path
}

// GetServerConfig returns the current server configuration for the Web UI.
func (s *Server) GetServerConfig() web.ServerConfigInfo {
	return web.ServerConfigInfo{
		TeamKey:    s.cfg.TeamKey,
		ListenAddr: s.cfg.Server.ListenAddr,
	}
}

// UpdateServerConfig updates the server's teamKey and/or listenAddr.
// teamKey changes take effect immediately; listenAddr changes require a restart.
func (s *Server) UpdateServerConfig(teamKey, listenAddr string) error {
	// Update teamKey — takes effect immediately.
	if teamKey != "" && teamKey != s.cfg.TeamKey {
		s.cfg.TeamKey = teamKey
		s.teamKeyHash = protocol.ComputeTeamKeyHash(teamKey)
	}

	// Update listenAddr — saved but requires restart to take effect.
	if listenAddr != "" {
		s.cfg.Server.ListenAddr = listenAddr
	}

	// Save to config file if path is known.
	if s.cfgPath != "" {
		return config.SaveConfig(s.cfgPath, s.cfg)
	}
	return nil
}

// sendModeString converts a uint8 send mode to a human-readable string.
func sendModeString(mode uint8) string {
	switch mode {
	case protocol.SendModeRedundant:
		return "redundant"
	case protocol.SendModeFailover:
		return "failover"
	default:
		return "unknown"
	}
}

// GetClients returns a summary list of all connected clients.
func (s *Server) GetClients() []web.ClientInfo {
	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	clients := make([]web.ClientInfo, 0, len(s.sessions))
	now := time.Now()
	for _, sess := range s.sessions {
		online := now.Sub(sess.LastSeen) < s.clientTimeout
		clients = append(clients, web.ClientInfo{
			ClientID:   sess.ClientID,
			VirtualIP:  sess.VirtualIP.String(),
			DeviceName: sess.DeviceName,
			SendMode:   sendModeString(sess.SendMode),
			Online:     online,
			LastSeen:   sess.LastSeen.Format(time.RFC3339),
			AddrCount:  len(sess.Addrs),
		})
	}
	sort.Slice(clients, func(i, j int) bool {
		return clients[i].ClientID < clients[j].ClientID
	})
	return clients
}

// GetClient returns detailed info for a single client, or nil if not found.
func (s *Server) GetClient(id uint16) *web.ClientDetailInfo {
	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}

	now := time.Now()
	online := now.Sub(sess.LastSeen) < s.clientTimeout

	addrs := make([]web.AddrDetail, 0, len(sess.Addrs))
	for _, ai := range sess.Addrs {
		addrs = append(addrs, web.AddrDetail{
			Addr:     ai.Addr.String(),
			LastSeen: ai.LastSeen.Format(time.RFC3339),
		})
	}

	return &web.ClientDetailInfo{
		ClientInfo: web.ClientInfo{
			ClientID:   sess.ClientID,
			VirtualIP:  sess.VirtualIP.String(),
			DeviceName: sess.DeviceName,
			SendMode:   sendModeString(sess.SendMode),
			Online:     online,
			LastSeen:   sess.LastSeen.Format(time.RFC3339),
			AddrCount:  len(sess.Addrs),
		},
		Addrs: addrs,
	}
}

// DeleteClient removes a client session and its route table entry.
// If auto-allocation is used, the IP mapping is also removed from the pool.
func (s *Server) DeleteClient(id uint16) error {
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("client %d not found", id)
	}

	// Remove route table entry.
	ip4 := sess.VirtualIP.To4()
	if ip4 != nil {
		var key [4]byte
		copy(key[:], ip4)
		s.routeLock.Lock()
		delete(s.routeTable, key)
		s.routeLock.Unlock()
	}

	// Remove from sessions.
	delete(s.sessions, id)

	// Remove from IP pool if auto-allocated.
	s.ipPoolLock.Lock()
	delete(s.ipPool, id)
	delete(s.ipPoolNames, id)
	s.saveIPPool()
	s.ipPoolLock.Unlock()

	return nil
}

// GetDevices returns all registered devices (from IP pool), merged with
// live session data for online status.
func (s *Server) GetDevices() []web.DeviceInfo {
	// Snapshot IP pool data.
	s.ipPoolLock.Lock()
	poolSnapshot := make(map[uint16]net.IP, len(s.ipPool))
	nameSnapshot := make(map[uint16]string, len(s.ipPoolNames))
	for k, v := range s.ipPool {
		poolSnapshot[k] = v
	}
	for k, v := range s.ipPoolNames {
		nameSnapshot[k] = v
	}
	s.ipPoolLock.Unlock()

	// Merge with live session data.
	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	now := time.Now()
	devices := make([]web.DeviceInfo, 0, len(poolSnapshot))
	for clientID, ip := range poolSnapshot {
		d := web.DeviceInfo{
			ClientID:   clientID,
			VirtualIP:  ip.String(),
			DeviceName: nameSnapshot[clientID],
		}
		if sess, ok := s.sessions[clientID]; ok {
			d.Online = now.Sub(sess.LastSeen) < s.clientTimeout
			d.SendMode = sendModeString(sess.SendMode)
			d.LastSeen = sess.LastSeen.Format(time.RFC3339)
			d.AddrCount = len(sess.Addrs)
			if sess.DeviceName != "" {
				d.DeviceName = sess.DeviceName
			}
			for _, ai := range sess.Addrs {
				if ai.NICName != "" {
					d.PathRTTs = append(d.PathRTTs, web.DevicePathRTT{
						Name:    ai.NICName,
						RTTms:   int(ai.NICRTTms),
						TxBytes: ai.NICTxBytes,
						RxBytes: ai.NICRxBytes,
					})
				}
			}
			d.RxBytes = sess.RxBytes
			d.TxBytes = sess.TxBytes
		}
		devices = append(devices, d)
	}
	sort.Slice(devices, func(i, j int) bool {
		return devices[i].ClientID < devices[j].ClientID
	})
	return devices
}

// UpdateDeviceIP changes the virtual IP assigned to a device.
// If the device is online, it updates the session, route table, and notifies the client.
func (s *Server) UpdateDeviceIP(clientID uint16, newIPStr string) error {
	newIP := net.ParseIP(newIPStr).To4()
	if newIP == nil {
		return fmt.Errorf("invalid IPv4 address: %s", newIPStr)
	}

	// Check subnet membership.
	if s.subnet != nil && !s.subnet.Contains(newIP) {
		return fmt.Errorf("IP %s is not within subnet %s", newIP, s.subnet)
	}

	// Check not server's own IP.
	var newKey [4]byte
	copy(newKey[:], newIP)
	if newKey == s.serverVirtualIP {
		return fmt.Errorf("cannot assign server's own IP")
	}

	// Phase 1: Update IP pool.
	s.ipPoolLock.Lock()
	oldIP, exists := s.ipPool[clientID]
	if !exists {
		s.ipPoolLock.Unlock()
		return fmt.Errorf("device %d not found", clientID)
	}
	// Check uniqueness.
	for cid, ip := range s.ipPool {
		if cid != clientID && ip.Equal(newIP) {
			s.ipPoolLock.Unlock()
			return fmt.Errorf("IP %s is already assigned to client %d", newIP, cid)
		}
	}
	s.ipPool[clientID] = newIP
	s.saveIPPool()
	s.ipPoolLock.Unlock()

	// Phase 2: Update route table.
	var oldKey [4]byte
	if oldIP4 := oldIP.To4(); oldIP4 != nil {
		copy(oldKey[:], oldIP4)
	}
	s.routeLock.Lock()
	delete(s.routeTable, oldKey)
	s.routeTable[newKey] = clientID
	s.routeLock.Unlock()

	// Phase 3: Update live session and collect addresses for notification.
	var addrsToNotify []*net.UDPAddr
	s.sessionsLock.Lock()
	if sess, ok := s.sessions[clientID]; ok {
		sess.VirtualIP = newIP
		sess.PrefixLen = s.prefixLen
		for _, ai := range sess.Addrs {
			addrsToNotify = append(addrsToNotify, ai.Addr)
		}
	}
	s.sessionsLock.Unlock()

	// Phase 4: Notify online client via HeartbeatAck.
	for _, addr := range addrsToNotify {
		s.sendHeartbeatAck(addr, newIP, s.prefixLen, protocol.AckStatusOK)
	}

	return nil
}

// DeleteDevice removes a device from the IP pool, its session, and route table.
// Next time the device connects, it will be auto-assigned a new IP.
func (s *Server) DeleteDevice(clientID uint16) error {
	// Phase 1: Remove from IP pool.
	s.ipPoolLock.Lock()
	oldIP, exists := s.ipPool[clientID]
	if !exists {
		s.ipPoolLock.Unlock()
		return fmt.Errorf("device %d not found", clientID)
	}
	delete(s.ipPool, clientID)
	delete(s.ipPoolNames, clientID)
	s.saveIPPool()
	s.ipPoolLock.Unlock()

	// Phase 2: Remove route table entry.
	if oldIP4 := oldIP.To4(); oldIP4 != nil {
		var key [4]byte
		copy(key[:], oldIP4)
		s.routeLock.Lock()
		delete(s.routeTable, key)
		s.routeLock.Unlock()
	}

	// Phase 3: Remove live session if exists.
	s.sessionsLock.Lock()
	if _, ok := s.sessions[clientID]; ok {
		delete(s.sessions, clientID)
		s.dedup.Reset(clientID)
	}
	s.sessionsLock.Unlock()

	return nil
}

// GetRoutes returns the current route table as a slice of RouteEntry.
func (s *Server) GetRoutes() []web.RouteEntry {
	s.routeLock.RLock()
	defer s.routeLock.RUnlock()

	routes := make([]web.RouteEntry, 0, len(s.routeTable))
	for ipBytes, clientID := range s.routeTable {
		vip := net.IP(ipBytes[:]).String()
		routes = append(routes, web.RouteEntry{
			VirtualIP: vip,
			ClientID:  clientID,
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].ClientID < routes[j].ClientID
	})
	return routes
}
