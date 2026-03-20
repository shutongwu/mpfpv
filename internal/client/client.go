package client

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/protocol"
	"github.com/cloud/mpfpv/internal/transport"
	"github.com/cloud/mpfpv/internal/tunnel"
	log "github.com/sirupsen/logrus"
)

const (
	heartbeatInterval = 1 * time.Second
	maxUDPPacketSize  = 65535
)

// Client is the mpfpv client with optional TUN integration.
type Client struct {
	cfg         *config.Config
	conn        *net.UDPConn
	serverAddr  *net.UDPAddr
	dedup       *protocol.Deduplicator
	teamKeyHash [8]byte
	virtualIP   net.IP
	prefixLen   uint8
	sendMode    uint8
	seq         uint32 // atomic; incremented per Send call
	registered  int32  // atomic bool; 1 = registered
	tunDev      tunnel.Device
	tunReady    chan struct{} // closed when TUN is configured (auto-assign mode)
	mu          sync.Mutex

	// Multi-path support (Phase 3).
	multipath    *transport.MultiPathSender // multi-path sender (nil when not used)
	useMultipath bool                       // true when multipath is active

	// RTT tracking: timestamp of last heartbeat sent, used to compute RTT
	// when the corresponding HeartbeatAck arrives.
	lastHeartbeatSent time.Time
	hbTimeMu          sync.Mutex
}

// New creates a new Client from the given config.
func New(cfg *config.Config) (*Client, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("client: client config section is nil")
	}
	cc := cfg.Client

	serverAddr, err := net.ResolveUDPAddr("udp", cc.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("client: resolve serverAddr %q: %w", cc.ServerAddr, err)
	}

	teamKeyHash := protocol.ComputeTeamKeyHash(cfg.TeamKey)

	var sendMode uint8
	switch cc.SendMode {
	case "failover":
		sendMode = protocol.SendModeFailover
	default:
		sendMode = protocol.SendModeRedundant
	}

	var virtualIP net.IP
	var prefixLen uint8
	if cc.VirtualIP != "" {
		ip, ipNet, err := net.ParseCIDR(cc.VirtualIP)
		if err != nil {
			return nil, fmt.Errorf("client: parse virtualIP %q: %w", cc.VirtualIP, err)
		}
		virtualIP = ip.To4()
		ones, _ := ipNet.Mask.Size()
		prefixLen = uint8(ones)
	} else {
		virtualIP = net.IPv4zero.To4()
		prefixLen = 0
	}

	dedupWindow := cc.DedupWindow
	if dedupWindow <= 0 {
		dedupWindow = protocol.DefaultWindowSize
	}

	c := &Client{
		cfg:         cfg,
		serverAddr:  serverAddr,
		dedup:       protocol.NewDeduplicator(dedupWindow),
		teamKeyHash: teamKeyHash,
		virtualIP:   virtualIP,
		prefixLen:   prefixLen,
		sendMode:    sendMode,
		tunReady:    make(chan struct{}),
	}

	return c, nil
}

// setupTUN creates and configures the TUN device. On failure it logs a warning
// and returns the error; the client continues in UDP-only mode.
func (c *Client) setupTUN() error {
	c.mu.Lock()
	vip := c.virtualIP
	pl := c.prefixLen
	c.mu.Unlock()

	mtu := c.cfg.Client.MTU
	if mtu <= 0 {
		mtu = tunnel.DefaultMTU
	}

	dev, err := tunnel.CreateTUN(tunnel.Config{
		Name:      tunnel.DefaultName,
		MTU:       mtu,
		VirtualIP: vip,
		PrefixLen: int(pl),
	})
	if err != nil {
		log.WithError(err).Warn("TUN creation failed, running in UDP-only mode")
		return err
	}
	c.tunDev = dev
	log.WithFields(log.Fields{
		"device":    dev.Name(),
		"virtualIP": vip.String(),
		"prefixLen": pl,
		"mtu":       mtu,
	}).Info("TUN device created")
	return nil
}

// Run starts the client. It blocks until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	// Try to create MultiPathSender for multi-NIC support.
	// Skip multipath when the server is on loopback — bound sockets (SO_BINDTODEVICE)
	// cannot reach 127.0.0.0/8 since loopback traffic stays on the lo interface.
	if !c.serverAddr.IP.IsLoopback() {
		mp, err := transport.NewMultiPathSender(c.serverAddr, c.sendMode, c.cfg.Client.ExcludedInterfaces)
		if err == nil {
			if startErr := mp.Start(); startErr == nil {
				c.multipath = mp
				c.useMultipath = true
				log.Info("client: multipath mode enabled")
			} else {
				log.WithError(startErr).Warn("client: multipath start failed, falling back to single socket")
			}
		} else {
			log.WithError(err).Warn("client: multipath init failed, falling back to single socket")
		}
	}

	// If multipath is not available, fall back to single UDP socket.
	if !c.useMultipath {
		conn, err := net.ListenUDP("udp", nil)
		if err != nil {
			return fmt.Errorf("client: listen UDP: %w", err)
		}
		c.conn = conn
		defer conn.Close()
	} else {
		defer c.multipath.Stop()
	}

	log.WithFields(log.Fields{
		"serverAddr":   c.serverAddr.String(),
		"clientID":     c.cfg.Client.ClientID,
		"virtualIP":    c.virtualIP.String(),
		"sendMode":     c.cfg.Client.SendMode,
		"useMultipath": c.useMultipath,
	}).Info("client starting")

	// Static IP mode: create TUN immediately.
	if c.virtualIP != nil && !c.virtualIP.Equal(net.IPv4zero) {
		c.setupTUN()
		close(c.tunReady)
	}

	// Start goroutines.
	recvCtx, recvCancel := context.WithCancel(ctx)
	defer recvCancel()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if c.useMultipath {
			c.recvLoopMultipath(recvCtx)
		} else {
			c.recvLoop(recvCtx)
		}
	}()
	go func() {
		defer wg.Done()
		c.heartbeatLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		c.tunReadLoop(ctx)
	}()

	<-ctx.Done()
	log.Info("client shutting down")
	if c.conn != nil {
		c.conn.Close() // unblock recvLoop
	}
	if c.tunDev != nil {
		c.tunDev.Close() // unblock tunReadLoop
	}
	wg.Wait()
	return nil
}

// tunReadLoop reads IP packets from the TUN device and sends them to the server.
func (c *Client) tunReadLoop(ctx context.Context) {
	// If TUN is not yet created (auto-assign mode), wait for tunReady.
	if c.tunDev == nil {
		select {
		case <-c.tunReady:
		case <-ctx.Done():
			return
		}
	}

	// After tunReady, tunDev may still be nil if setupTUN failed.
	if c.tunDev == nil {
		return
	}

	mtu := c.cfg.Client.MTU
	if mtu <= 0 {
		mtu = tunnel.DefaultMTU
	}

	buf := make([]byte, mtu+protocol.HeaderSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := c.tunDev.Read(buf[protocol.HeaderSize:])
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.WithError(err).Warn("TUN read error")
			return
		}
		if n < 1 {
			continue
		}

		// Encapsulate with header.
		seq := atomic.AddUint32(&c.seq, 1) - 1
		hdr := &protocol.Header{
			Version:  protocol.Version1,
			Type:     protocol.TypeData,
			ClientID: c.cfg.Client.ClientID,
			Seq:      seq,
		}
		protocol.EncodeHeader(buf, hdr)

		// Send to server via multipath or single socket.
		pkt := buf[:protocol.HeaderSize+n]
		if c.useMultipath {
			if err := c.multipath.Send(pkt); err != nil {
				log.WithError(err).Warn("failed to send TUN packet to server (multipath)")
			}
		} else {
			if _, err := c.conn.WriteToUDP(pkt, c.serverAddr); err != nil {
				log.WithError(err).Warn("failed to send TUN packet to server")
			}
		}
	}
}

// heartbeatLoop sends heartbeats at a fixed interval. The first heartbeat
// is sent immediately.
func (c *Client) heartbeatLoop(ctx context.Context) {
	// Send the first heartbeat immediately.
	if err := c.sendHeartbeat(); err != nil {
		log.WithError(err).Warn("failed to send initial heartbeat")
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.sendHeartbeat(); err != nil {
				log.WithError(err).Warn("failed to send heartbeat")
			}
		}
	}
}

// sendHeartbeat encodes and sends a single heartbeat packet to the server.
// In multipath mode, heartbeats are sent via ALL paths (SendAll) so the
// server learns every source address and each path gets RTT probed.
func (c *Client) sendHeartbeat() error {
	buf := make([]byte, protocol.HeaderSize+protocol.HeartbeatPayloadSize)

	seq := atomic.AddUint32(&c.seq, 1) - 1 // first seq = 0

	hdr := &protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeHeartbeat,
		ClientID: c.cfg.Client.ClientID,
		Seq:      seq,
	}
	protocol.EncodeHeader(buf, hdr)

	c.mu.Lock()
	hb := &protocol.HeartbeatPayload{
		VirtualIP:   c.virtualIP,
		PrefixLen:   c.prefixLen,
		SendMode:    c.sendMode,
		TeamKeyHash: c.teamKeyHash,
	}
	c.mu.Unlock()
	protocol.EncodeHeartbeat(buf[protocol.HeaderSize:], hb)

	// Record send time for RTT measurement.
	c.hbTimeMu.Lock()
	c.lastHeartbeatSent = time.Now()
	c.hbTimeMu.Unlock()

	if c.useMultipath {
		// Send heartbeat through ALL paths so the server learns every
		// source address and we can measure per-path RTT.
		if err := c.multipath.SendAll(buf); err != nil {
			return fmt.Errorf("send heartbeat (multipath): %w", err)
		}
	} else {
		if _, err := c.conn.WriteToUDP(buf, c.serverAddr); err != nil {
			return fmt.Errorf("send heartbeat: %w", err)
		}
	}

	log.WithField("seq", seq).Debug("heartbeat sent")
	return nil
}

// recvLoop reads packets from the single UDP socket and dispatches them.
func (c *Client) recvLoop(ctx context.Context) {
	buf := make([]byte, maxUDPPacketSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			// Check if we are shutting down.
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.WithError(err).Warn("recv error")
			continue
		}

		if n < protocol.HeaderSize {
			log.WithField("size", n).Warn("received packet too short, dropping")
			continue
		}

		hdr, err := protocol.DecodeHeader(buf[:n])
		if err != nil {
			log.WithError(err).Warn("failed to decode header")
			continue
		}

		payload := buf[protocol.HeaderSize:n]

		switch hdr.Type {
		case protocol.TypeHeartbeatAck:
			c.handleHeartbeatAck(hdr, payload)
		case protocol.TypeData:
			c.handleData(hdr, payload)
		default:
			log.WithField("type", hdr.Type).Warn("received unknown packet type")
		}
	}
}

// recvLoopMultipath reads packets from the MultiPathSender's receive channel
// and dispatches them. It also tracks per-path RTT from HeartbeatAck responses.
func (c *Client) recvLoopMultipath(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-c.multipath.RecvChan():
			if !ok {
				return
			}

			if len(pkt.Data) < protocol.HeaderSize {
				log.WithField("size", len(pkt.Data)).Warn("received packet too short, dropping")
				continue
			}

			hdr, err := protocol.DecodeHeader(pkt.Data)
			if err != nil {
				log.WithError(err).Warn("failed to decode header")
				continue
			}

			payload := pkt.Data[protocol.HeaderSize:]

			switch hdr.Type {
			case protocol.TypeHeartbeatAck:
				c.handleHeartbeatAck(hdr, payload)

				// Update RTT for this path based on time since last heartbeat.
				c.hbTimeMu.Lock()
				sentAt := c.lastHeartbeatSent
				c.hbTimeMu.Unlock()
				if !sentAt.IsZero() {
					rtt := time.Since(sentAt)
					c.multipath.UpdateRTT(pkt.FromPath, rtt)
				}

			case protocol.TypeData:
				c.handleData(hdr, payload)

			default:
				log.WithField("type", hdr.Type).Warn("received unknown packet type")
			}
		}
	}
}

// handleHeartbeatAck processes a HeartbeatAck from the server.
func (c *Client) handleHeartbeatAck(hdr protocol.Header, payload []byte) {
	ack, err := protocol.DecodeHeartbeatAck(payload)
	if err != nil {
		log.WithError(err).Warn("failed to decode heartbeat ack")
		return
	}

	switch ack.Status {
	case protocol.AckStatusOK:
		wasRegistered := atomic.LoadInt32(&c.registered) == 1

		c.mu.Lock()
		c.virtualIP = ack.AssignedIP.To4()
		c.prefixLen = ack.PrefixLen
		c.mu.Unlock()
		atomic.StoreInt32(&c.registered, 1)

		log.WithFields(log.Fields{
			"virtualIP": ack.AssignedIP.String(),
			"prefixLen": ack.PrefixLen,
		}).Info("registered with server")

		// Auto-assign mode: first OK ack, TUN not yet created.
		if !wasRegistered && c.tunDev == nil {
			c.setupTUN()
			close(c.tunReady) // signal tunReadLoop to start
		}

	case protocol.AckStatusTeamKeyMismatch:
		log.Error("heartbeat ack: teamKey mismatch — check config")

	case protocol.AckStatusClientIDConflict:
		log.Error("heartbeat ack: clientID conflict — another client is using the same ID")

	default:
		log.WithField("status", ack.Status).Warn("heartbeat ack: unknown status")
	}
}

// handleData processes a Data packet from the server.
func (c *Client) handleData(hdr protocol.Header, payload []byte) {
	if c.dedup.IsDuplicate(hdr.ClientID, hdr.Seq) {
		log.WithFields(log.Fields{
			"clientID": hdr.ClientID,
			"seq":      hdr.Seq,
		}).Debug("duplicate data packet, dropping")
		return
	}

	log.WithFields(log.Fields{
		"clientID":    hdr.ClientID,
		"seq":         hdr.Seq,
		"payloadSize": len(payload),
	}).Debug("received data packet")

	// Phase 2: write to TUN if available.
	if c.tunDev != nil {
		if _, err := c.tunDev.Write(payload); err != nil {
			log.WithError(err).Warn("failed to write to TUN")
		}
	}
}

// Send encapsulates payload as a Data packet and sends it to the server.
// It is safe for concurrent use.
func (c *Client) Send(payload []byte) error {
	buf := make([]byte, protocol.HeaderSize+len(payload))

	seq := atomic.AddUint32(&c.seq, 1) - 1

	hdr := &protocol.Header{
		Version:  protocol.Version1,
		Type:     protocol.TypeData,
		ClientID: c.cfg.Client.ClientID,
		Seq:      seq,
	}
	protocol.EncodeHeader(buf, hdr)
	copy(buf[protocol.HeaderSize:], payload)

	if c.useMultipath {
		if err := c.multipath.Send(buf); err != nil {
			return fmt.Errorf("client send (multipath): %w", err)
		}
		return nil
	}
	_, err := c.conn.WriteToUDP(buf, c.serverAddr)
	if err != nil {
		return fmt.Errorf("client send: %w", err)
	}
	return nil
}

// IsRegistered returns whether the client has received a successful HeartbeatAck.
func (c *Client) IsRegistered() bool {
	return atomic.LoadInt32(&c.registered) == 1
}

// Multipath returns the MultiPathSender if multipath mode is active, or nil.
func (c *Client) Multipath() *transport.MultiPathSender {
	return c.multipath
}
