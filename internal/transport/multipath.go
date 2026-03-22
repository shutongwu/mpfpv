package transport

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cloud/mpfpv/internal/protocol"
	log "github.com/sirupsen/logrus"
)

const (
	// Default number of RTT samples for the sliding window.
	defaultRTTSamples = 10

	// Default switch cooldown to prevent ping-pong.
	defaultSwitchCooldown = 5 * time.Second

	// Number of consecutive heartbeat misses before marking a path as Down.
	missThresholdDown = 5

	// recvChanSize is the buffer size for the receive channel.
	recvChanSize = 256

	// recvBufSize is the per-goroutine receive buffer size.
	recvBufSize = 65535
)

// PathStatus represents the health state of a network path.
type PathStatus int

const (
	PathActive  PathStatus = iota
	PathSuspect            // write error occurred, not yet confirmed dead
	PathDown               // consecutive heartbeat misses exceeded threshold
)

// String returns a human-readable representation of PathStatus.
func (s PathStatus) String() string {
	switch s {
	case PathActive:
		return "active"
	case PathSuspect:
		return "suspect"
	case PathDown:
		return "down"
	default:
		return "unknown"
	}
}

// Path represents a single network path through one interface.
type Path struct {
	IfaceName  string
	LocalAddr  net.IP
	Conn       *net.UDPConn
	RTT        time.Duration   // sliding-window average RTT
	rttSamples []time.Duration // last N RTT samples
	LastRecv   time.Time       // last time a packet was received on this path
	Status     PathStatus
	missCount  int // consecutive heartbeat misses
	TxBytes    uint64 // bytes sent through this path
	mu         sync.Mutex
	closed     chan struct{} // closed when path is deliberately removed; used by perPathRecvLoop
}

// RecvPacket is a packet received from the server on any path.
type RecvPacket struct {
	Data     []byte
	FromPath string // interface name the packet arrived on
	Addr     *net.UDPAddr
}

// PathInfo is a read-only snapshot of a Path for external consumers (Web UI).
type PathInfo struct {
	IfaceName string
	LocalAddr string
	RTT       time.Duration
	LastRecv  time.Time
	Status    string
	IsActive  bool // true if this is the active path in failover mode
	TxBytes   uint64
}

// MultiPathSender manages sending data over multiple network interfaces.
type MultiPathSender struct {
	serverAddr     *net.UDPAddr
	paths          map[string]*Path // ifaceName -> Path
	sendMode       uint8            // protocol.SendModeRedundant or SendModeFailover
	activePath     string           // failover mode: current best path
	lastSwitch     time.Time        // anti-ping-pong: last path switch time
	switchCooldown time.Duration
	recvCh         chan RecvPacket
	recvConn       *net.UDPConn     // central unbound recv socket (NIC-independent)
	recvPort       uint16           // port of recvConn, sent in heartbeat as ReplyPort
	mu             sync.RWMutex
	watcher        *InterfaceWatcher
	stopCh         chan struct{}
	wg             sync.WaitGroup
	OnPathChange   func() // called when paths are added; used to trigger immediate heartbeat
}

// NewMultiPathSender creates a new multi-path sender targeting serverAddr.
func NewMultiPathSender(serverAddr *net.UDPAddr, sendMode uint8, excluded []string) (*MultiPathSender, error) {
	if serverAddr == nil {
		return nil, fmt.Errorf("transport: serverAddr must not be nil")
	}

	m := &MultiPathSender{
		serverAddr:     serverAddr,
		paths:          make(map[string]*Path),
		sendMode:       sendMode,
		switchCooldown: defaultSwitchCooldown,
		recvCh:         make(chan RecvPacket, recvChanSize),
		stopCh:         make(chan struct{}),
	}

	m.watcher = NewInterfaceWatcher(excluded, m.onInterfaceChange)
	return m, nil
}

// Start begins interface monitoring and creates initial paths.
func (m *MultiPathSender) Start() error {
	// Create central unbound receive socket. This socket is not tied to any
	// NIC, so it survives NIC removal events without interruption.
	recvConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fmt.Errorf("transport: create recv socket: %w", err)
	}
	m.recvConn = recvConn
	m.recvPort = uint16(recvConn.LocalAddr().(*net.UDPAddr).Port)
	log.WithField("port", m.recvPort).Info("central recv socket created")

	// Start central receive loop.
	m.wg.Add(1)
	go m.centralRecvLoop()

	if err := m.watcher.Start(); err != nil {
		recvConn.Close()
		return fmt.Errorf("transport: start watcher: %w", err)
	}

	// Create paths for all currently available interfaces.
	current := m.watcher.Current()
	for _, info := range current {
		m.addPath(info)
	}

	// Select initial active path for failover mode.
	if m.sendMode == protocol.SendModeFailover {
		m.mu.Lock()
		m.activePath = m.selectBestPathLocked()
		m.mu.Unlock()
	}

	return nil
}

// RecvPort returns the central receive socket's port number.
// The client should include this in heartbeat ReplyPort so the server
// sends data to this port instead of the per-NIC send socket ports.
func (m *MultiPathSender) RecvPort() uint16 {
	return m.recvPort
}

// Stop shuts down all paths and the interface watcher.
func (m *MultiPathSender) Stop() {
	select {
	case <-m.stopCh:
		return
	default:
		close(m.stopCh)
	}

	m.watcher.Stop()

	m.mu.Lock()
	for name, p := range m.paths {
		p.Conn.Close()
		delete(m.paths, name)
	}
	m.mu.Unlock()

	if m.recvConn != nil {
		m.recvConn.Close()
	}

	m.wg.Wait()
}

// Send sends data through the configured send mode.
// In redundant mode all active/suspect paths get a copy.
// In failover mode only the best path is used.
func (m *MultiPathSender) Send(data []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.paths) == 0 {
		return fmt.Errorf("transport: no available paths")
	}

	if m.sendMode == protocol.SendModeFailover {
		return m.sendFailoverLocked(data)
	}
	return m.sendRedundantLocked(data)
}

// SendAll sends data through ALL paths (including suspect), regardless of send
// mode. Used for heartbeats that need to probe every path.
func (m *MultiPathSender) SendAll(data []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var lastErr error
	for _, p := range m.paths {
		p.mu.Lock()
		status := p.Status
		p.mu.Unlock()
		if status == PathDown {
			continue
		}
		if _, err := p.Conn.WriteToUDP(data, m.serverAddr); err != nil {
			p.mu.Lock()
			p.Status = PathSuspect
			p.mu.Unlock()
			lastErr = err
		}
	}
	return lastErr
}

// RecvChan returns the channel on which received packets are delivered.
func (m *MultiPathSender) RecvChan() <-chan RecvPacket {
	return m.recvCh
}

// UpdateRTT records a new RTT sample for the given interface and updates the
// sliding average. It also resets the miss counter and marks the path Active.
func (m *MultiPathSender) UpdateRTT(ifaceName string, rtt time.Duration) {
	m.mu.RLock()
	p, ok := m.paths[ifaceName]
	m.mu.RUnlock()
	if !ok {
		return
	}

	p.mu.Lock()
	p.rttSamples = append(p.rttSamples, rtt)
	if len(p.rttSamples) > defaultRTTSamples {
		p.rttSamples = p.rttSamples[len(p.rttSamples)-defaultRTTSamples:]
	}
	p.RTT = averageDuration(p.rttSamples)
	p.LastRecv = time.Now()
	p.missCount = 0
	p.Status = PathActive
	p.mu.Unlock()

	// In failover mode, re-evaluate best path.
	if m.sendMode == protocol.SendModeFailover {
		m.mu.Lock()
		m.activePath = m.selectBestPathLocked()
		m.mu.Unlock()
	}
}

// IncrementMiss increments the heartbeat miss counter for a path. If it
// exceeds the threshold the path is marked Down.
func (m *MultiPathSender) IncrementMiss(ifaceName string) {
	m.mu.RLock()
	p, ok := m.paths[ifaceName]
	m.mu.RUnlock()
	if !ok {
		return
	}

	p.mu.Lock()
	p.missCount++
	if p.missCount >= missThresholdDown {
		p.Status = PathDown
		log.WithField("iface", ifaceName).Warn("path marked down (heartbeat miss)")
	}
	p.mu.Unlock()

	// In failover mode, switch away from a down path immediately.
	if m.sendMode == protocol.SendModeFailover {
		m.mu.Lock()
		m.activePath = m.selectBestPathLocked()
		m.mu.Unlock()
	}
}

// GetPaths returns a snapshot of all paths for external display.
func (m *MultiPathSender) GetPaths() []PathInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]PathInfo, 0, len(m.paths))
	for _, p := range m.paths {
		p.mu.Lock()
		pi := PathInfo{
			IfaceName: p.IfaceName,
			LocalAddr: p.LocalAddr.String(),
			RTT:       p.RTT,
			LastRecv:  p.LastRecv,
			Status:    p.Status.String(),
			IsActive:  p.IfaceName == m.activePath,
			TxBytes:   p.TxBytes,
		}
		p.mu.Unlock()
		out = append(out, pi)
	}
	return out
}

// PathNames returns the names of all current paths.
func (m *MultiPathSender) PathNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.paths))
	for name := range m.paths {
		names = append(names, name)
	}
	return names
}

// SetSendMode changes the send mode at runtime (redundant or failover).
func (m *MultiPathSender) SetSendMode(mode uint8) {
	m.mu.Lock()
	m.sendMode = mode
	if mode == protocol.SendModeFailover {
		m.activePath = m.selectBestPathLocked()
	}
	m.mu.Unlock()
}

// --- internal methods ---

func (m *MultiPathSender) sendRedundantLocked(data []byte) error {
	// Serial direct WriteToUDP to each active path. SO_SNDTIMEO (50ms)
	// on the socket ensures a dying NIC cannot block for more than 50ms,
	// so the total worst-case latency is 50ms * number_of_paths.
	sentCount := 0
	for _, p := range m.paths {
		p.mu.Lock()
		status := p.Status
		p.mu.Unlock()
		if status == PathDown {
			continue
		}
		if _, err := p.Conn.WriteToUDP(data, m.serverAddr); err != nil {
			p.mu.Lock()
			wasSuspect := p.Status == PathSuspect
			p.Status = PathSuspect
			p.mu.Unlock()
			if !wasSuspect {
				log.Warnf("multipath: send via %s failed: %v", p.IfaceName, err)
			}
			continue
		}
		p.mu.Lock()
		p.TxBytes += uint64(len(data))
		p.mu.Unlock()
		sentCount++
	}
	if sentCount == 0 {
		return fmt.Errorf("transport: all paths failed or down")
	}
	return nil
}

func (m *MultiPathSender) sendFailoverLocked(data []byte) error {
	name := m.activePath
	p, ok := m.paths[name]
	if !ok {
		// No active path — try any Active path.
		for _, pp := range m.paths {
			pp.mu.Lock()
			s := pp.Status
			pp.mu.Unlock()
			if s != PathDown {
				p = pp
				break
			}
		}
		if p == nil {
			return fmt.Errorf("transport: no active path available")
		}
	}

	if _, err := p.Conn.WriteToUDP(data, m.serverAddr); err != nil {
		p.mu.Lock()
		p.Status = PathSuspect
		p.mu.Unlock()
		return err
	}
	p.mu.Lock()
	p.TxBytes += uint64(len(data))
	p.mu.Unlock()
	return nil
}

// selectBestPathLocked picks the best path for failover mode.
// Must be called with m.mu held (write lock).
func (m *MultiPathSender) selectBestPathLocked() string {
	var bestName string
	var bestRTT time.Duration

	for name, p := range m.paths {
		p.mu.Lock()
		status := p.Status
		rtt := p.RTT
		p.mu.Unlock()

		if status == PathDown {
			continue
		}
		if bestName == "" || rtt < bestRTT {
			bestName = name
			bestRTT = rtt
		}
	}

	// Anti-ping-pong: if the current active path is still not down and we
	// are within the cooldown period, keep the current path.
	if m.activePath != "" && m.activePath != bestName {
		if curPath, ok := m.paths[m.activePath]; ok {
			curPath.mu.Lock()
			curStatus := curPath.Status
			curPath.mu.Unlock()
			if curStatus != PathDown && time.Since(m.lastSwitch) < m.switchCooldown {
				return m.activePath
			}
		}
	}

	if bestName != m.activePath {
		log.WithFields(log.Fields{
			"from": m.activePath,
			"to":   bestName,
		}).Info("failover: switching active path")
		m.lastSwitch = time.Now()
	}

	return bestName
}

// onInterfaceChange is called by InterfaceWatcher when interfaces change.
func (m *MultiPathSender) onInterfaceChange(added, removed []InterfaceInfo) {
	for _, info := range removed {
		m.removePath(info.Name)
	}
	for i := range added {
		m.addPath(&added[i])
	}
	// Re-evaluate active path.
	if m.sendMode == protocol.SendModeFailover {
		m.mu.Lock()
		m.activePath = m.selectBestPathLocked()
		m.mu.Unlock()
	}
	// Trigger immediate heartbeat so server learns new addresses fast.
	if len(added) > 0 && m.OnPathChange != nil {
		m.OnPathChange()
	}
}

// addPath creates a new Path for the given interface info.
func (m *MultiPathSender) addPath(info *InterfaceInfo) {
	if len(info.Addrs) == 0 {
		return
	}
	localAddr := info.Addrs[0] // Use the first IPv4 address.

	conn, err := createBoundUDPConn(localAddr, info.Name)
	if err != nil {
		log.WithFields(log.Fields{
			"iface": info.Name,
			"addr":  localAddr.String(),
		}).WithError(err).Warn("failed to create bound socket, skipping interface")
		return
	}

	p := &Path{
		IfaceName: info.Name,
		LocalAddr: localAddr,
		Conn:      conn,
		Status:    PathActive,
		LastRecv:  time.Now(),
		closed:    make(chan struct{}),
	}

	m.mu.Lock()
	m.paths[info.Name] = p
	m.mu.Unlock()

	log.WithFields(log.Fields{
		"iface": info.Name,
		"addr":  localAddr.String(),
	}).Info("path added")

	// Start receive goroutine for this path (NAT compatibility).
	m.wg.Add(1)
	go m.perPathRecvLoop(p)
}

// removePath closes and removes the path for the named interface.
// Like engarde: close the socket FIRST to unblock any in-flight WriteToUDP
// in sendRedundantLocked (which holds RLock), then acquire WLock to delete.
func (m *MultiPathSender) removePath(name string) {
	// Look up the path with a read lock (fast, doesn't block sends).
	m.mu.RLock()
	p, ok := m.paths[name]
	m.mu.RUnlock()
	if !ok {
		return
	}

	// Close socket FIRST. This unblocks any in-flight WriteToUDP (which
	// holds only RLock), causing it to return an error immediately.
	// Also signals perPathRecvLoop to exit via the closed channel.
	close(p.closed)
	p.Conn.Close()

	// Now acquire write lock and remove from map.
	m.mu.Lock()
	delete(m.paths, name)
	m.mu.Unlock()

	log.WithField("iface", name).Info("path removed")
}

// perPathRecvLoop reads from a per-NIC socket. Needed because NAT mappings
// point to the per-NIC port. Retries on transient errors instead of exiting.
func (m *MultiPathSender) perPathRecvLoop(p *Path) {
	defer m.wg.Done()
	buf := make([]byte, recvBufSize)
	for {
		n, addr, err := p.Conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-m.stopCh:
				return
			case <-p.closed:
				return
			default:
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case m.recvCh <- RecvPacket{Data: data, FromPath: p.IfaceName, Addr: addr}:
		case <-m.stopCh:
			return
		}
	}
}

// centralRecvLoop reads packets from the central unbound receive socket.
// This socket is not tied to any NIC, so it survives NIC removal events.
func (m *MultiPathSender) centralRecvLoop() {
	defer m.wg.Done()
	buf := make([]byte, recvBufSize)
	for {
		n, addr, err := m.recvConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-m.stopCh:
				return
			default:
			}
			log.Debugf("centralRecvLoop: read error (retrying): %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		select {
		case m.recvCh <- RecvPacket{
			Data:     data,
			FromPath: "central",
			Addr:     addr,
		}:
		case <-m.stopCh:
			return
		}
	}
}

// AddPathForTest adds a path manually for testing purposes.
// The caller is responsible for closing the connection.
func (m *MultiPathSender) AddPathForTest(name string, localAddr net.IP, conn *net.UDPConn) {
	p := &Path{
		IfaceName: name,
		LocalAddr: localAddr,
		Conn:      conn,
		Status:    PathActive,
		LastRecv:  time.Now(),
		closed:    make(chan struct{}),
	}
	m.mu.Lock()
	m.paths[name] = p
	m.mu.Unlock()
}

// InjectRecvForTest pushes a packet into the receive channel for testing.
func (m *MultiPathSender) InjectRecvForTest(pkt RecvPacket) {
	m.recvCh <- pkt
}

// averageDuration computes the arithmetic mean of a duration slice.
func averageDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}
