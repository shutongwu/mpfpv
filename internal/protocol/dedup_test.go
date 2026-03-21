package protocol

import (
	"math"
	"testing"
)

func TestDedupNormalSequence(t *testing.T) {
	d := NewDeduplicator(64)
	for i := uint32(0); i < 100; i++ {
		if d.IsDuplicate(1, i) {
			t.Fatalf("seq %d should not be duplicate", i)
		}
	}
}

func TestDedupExactDuplicate(t *testing.T) {
	d := NewDeduplicator(64)
	if d.IsDuplicate(1, 5) {
		t.Fatal("first should not be dup")
	}
	if !d.IsDuplicate(1, 5) {
		t.Fatal("second should be dup")
	}
}

func TestDedupOutOfOrder(t *testing.T) {
	d := NewDeduplicator(64)
	// Send 0, 2, 1 — all should be new
	if d.IsDuplicate(1, 0) {
		t.Fatal("seq 0 should not be dup")
	}
	if d.IsDuplicate(1, 2) {
		t.Fatal("seq 2 should not be dup")
	}
	if d.IsDuplicate(1, 1) {
		t.Fatal("seq 1 should not be dup (out of order)")
	}
	// Now replay 1
	if !d.IsDuplicate(1, 1) {
		t.Fatal("seq 1 replay should be dup")
	}
}

func TestDedupWindowSlide(t *testing.T) {
	ws := 64
	d := NewDeduplicator(ws)

	// Accept seq 0
	d.IsDuplicate(1, 0)
	// Jump to seq 100 (beyond window)
	if d.IsDuplicate(1, 100) {
		t.Fatal("seq 100 should not be dup")
	}
	// seq 0 is far behind (100 - 0 = 100 > 64), treated as restart → reset and accept
	if d.IsDuplicate(1, 0) {
		t.Fatal("seq 0 should not be dup (treated as restart)")
	}
	// seq within window (100-63 = 37) should be new
	if d.IsDuplicate(1, 37) {
		t.Fatal("seq 37 should not be dup (within window)")
	}
}

func TestDedupTooOld(t *testing.T) {
	d := NewDeduplicator(64)
	d.IsDuplicate(1, 100)
	// seq 30 is 70 behind maxSeq=100, > windowSize=64 → treated as restart
	if d.IsDuplicate(1, 30) {
		t.Fatal("seq 30 should not be dup (treated as restart)")
	}
}

func TestDedupMultipleClients(t *testing.T) {
	d := NewDeduplicator(64)
	// Client 1 and Client 2 are independent
	if d.IsDuplicate(1, 5) {
		t.Fatal("client1 seq5 should not be dup")
	}
	if d.IsDuplicate(2, 5) {
		t.Fatal("client2 seq5 should not be dup (independent)")
	}
	if !d.IsDuplicate(1, 5) {
		t.Fatal("client1 seq5 replay should be dup")
	}
	if !d.IsDuplicate(2, 5) {
		t.Fatal("client2 seq5 replay should be dup")
	}
}

func TestDedupSeqWraparound(t *testing.T) {
	d := NewDeduplicator(64)

	// Start near uint32 max
	start := uint32(math.MaxUint32 - 10)
	for i := uint32(0); i < 20; i++ {
		seq := start + i // will wrap around uint32
		if d.IsDuplicate(1, seq) {
			t.Fatalf("seq %d (i=%d) should not be dup", seq, i)
		}
	}

	// Replay a wrapped seq should be dup
	if !d.IsDuplicate(1, start+5) {
		t.Fatal("replay of wrapped seq should be dup")
	}

	// The current maxSeq is start+19 (wrapped).
	// start-1 is only 21 behind maxSeq, within window=64, and unseen => new.
	// Test a seq that is truly too old (more than 64 behind maxSeq).
	tooOld := start - 100 // well outside window → treated as restart
	if d.IsDuplicate(1, tooOld) {
		t.Fatal("seq far before start should not be dup (treated as restart)")
	}
}

func TestDedupDefaultWindowSize(t *testing.T) {
	d := NewDeduplicator(0)
	if d.windowSize != DefaultWindowSize {
		t.Fatalf("expected default window size %d, got %d", DefaultWindowSize, d.windowSize)
	}
}

func TestDedupLargeJump(t *testing.T) {
	d := NewDeduplicator(64)
	d.IsDuplicate(1, 0)
	// Jump far ahead — should clear bitmap and accept
	if d.IsDuplicate(1, 10000) {
		t.Fatal("large jump should not be dup")
	}
	// seq 0 is far behind 10000 (> window) → treated as restart, reset and accept
	if d.IsDuplicate(1, 0) {
		t.Fatal("seq 0 after large jump should not be dup (treated as restart)")
	}
}

func TestDedupBitmapReuse(t *testing.T) {
	// Ensure bitmap slots are properly cleared when window slides
	ws := 8
	d := NewDeduplicator(ws)

	// Fill window: 0..7
	for i := uint32(0); i < 8; i++ {
		d.IsDuplicate(1, i)
	}
	// Slide to 8: clears slot for seq 8%8=0
	if d.IsDuplicate(1, 8) {
		t.Fatal("seq 8 should not be dup")
	}
	// seq 0 is behind by 8 (= windowSize) → treated as restart, reset and accept
	if d.IsDuplicate(1, 0) {
		t.Fatal("seq 0 should not be dup after slide (treated as restart)")
	}
	// Slide further and verify new seqs work
	if d.IsDuplicate(1, 15) {
		t.Fatal("seq 15 should not be dup")
	}
}
