package protocol

import "sync"

// clientState holds per-clientID deduplication state.
type clientState struct {
	mu     sync.Mutex // per-client lock, eliminates cross-client contention
	maxSeq uint32
	bitmap []uint64 // bit array, indexed by seq % windowSize
	inited bool
}

// Deduplicator performs per-clientID sliding-window sequence deduplication.
type Deduplicator struct {
	mu         sync.RWMutex // protects the clients map only
	windowSize uint32
	clients    map[uint16]*clientState
}

// NewDeduplicator creates a Deduplicator with the given window size.
// If windowSize <= 0, DefaultWindowSize is used.
func NewDeduplicator(windowSize int) *Deduplicator {
	if windowSize <= 0 {
		windowSize = DefaultWindowSize
	}
	return &Deduplicator{
		windowSize: uint32(windowSize),
		clients:    make(map[uint16]*clientState),
	}
}

// IsDuplicate returns true if the packet should be discarded (duplicate),
// false if it is a new packet that should be processed.
func (d *Deduplicator) IsDuplicate(clientID uint16, seq uint32) bool {
	// Fast path: read-lock map lookup.
	d.mu.RLock()
	cs, ok := d.clients[clientID]
	d.mu.RUnlock()

	if !ok {
		// Slow path: write-lock to insert new client.
		d.mu.Lock()
		cs, ok = d.clients[clientID]
		if !ok {
			cs = &clientState{
				bitmap: make([]uint64, (d.windowSize+63)/64),
			}
			d.clients[clientID] = cs
		}
		d.mu.Unlock()
	}

	// Per-client lock: different clients never contend.
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if !cs.inited {
		cs.inited = true
		cs.maxSeq = seq
		cs.setBit(seq, d.windowSize)
		return false
	}

	// Calculate distance with uint32 wrap-around awareness.
	// We treat sequences within windowSize ahead as "new/ahead",
	// and use signed interpretation for wrap-around.
	diff := int64(seq) - int64(cs.maxSeq)

	// Handle uint32 wraparound: if diff magnitude is close to 2^32,
	// adjust accordingly.
	if diff > int64(1<<31) {
		// seq wrapped backward relative to maxSeq; treat as old
		diff -= int64(1 << 32)
	} else if diff < -int64(1<<31) {
		// maxSeq wrapped; seq is actually ahead
		diff += int64(1 << 32)
	}

	if diff > 0 {
		// seq is ahead of maxSeq — slide window forward
		shift := diff
		if shift >= int64(d.windowSize) {
			// clear entire bitmap
			for i := range cs.bitmap {
				cs.bitmap[i] = 0
			}
		} else {
			// clear bits that are being shifted out
			for i := int64(1); i <= shift; i++ {
				cs.clearBit(cs.maxSeq+uint32(i), d.windowSize)
			}
		}
		cs.maxSeq = seq
		cs.setBit(seq, d.windowSize)
		return false
	}

	if diff == 0 {
		// exact duplicate of maxSeq
		return true
	}

	// diff < 0: seq is behind maxSeq
	behind := -diff
	if behind >= int64(d.windowSize) {
		// Too far behind — likely a sender restart (seq wrapped back to 0).
		// Reset state and accept this packet as the start of a new sequence.
		cs.inited = false
		for i := range cs.bitmap {
			cs.bitmap[i] = 0
		}
		cs.inited = true
		cs.maxSeq = seq
		cs.setBit(seq, d.windowSize)
		return false
	}

	// Check bitmap
	if cs.getBit(seq, d.windowSize) {
		return true
	}
	cs.setBit(seq, d.windowSize)
	return false
}

// Reset clears dedup state for a clientID so that reconnecting clients
// are not falsely rejected when their seq counter restarts from zero.
func (d *Deduplicator) Reset(clientID uint16) {
	d.mu.Lock()
	delete(d.clients, clientID)
	d.mu.Unlock()
}

func (cs *clientState) setBit(seq uint32, windowSize uint32) {
	idx := seq % windowSize
	cs.bitmap[idx/64] |= 1 << (idx % 64)
}

func (cs *clientState) clearBit(seq uint32, windowSize uint32) {
	idx := seq % windowSize
	cs.bitmap[idx/64] &^= 1 << (idx % 64)
}

func (cs *clientState) getBit(seq uint32, windowSize uint32) bool {
	idx := seq % windowSize
	return cs.bitmap[idx/64]&(1<<(idx%64)) != 0
}
