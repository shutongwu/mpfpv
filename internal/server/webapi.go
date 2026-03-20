package server

import (
	"fmt"
	"net"
	"time"

	"github.com/cloud/mpfpv/internal/protocol"
	"github.com/cloud/mpfpv/internal/web"
)

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
	return routes
}
