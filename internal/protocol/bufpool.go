package protocol

import "sync"

// DefaultBufSize is the default capacity for pooled buffers.
// Sized to fit MTU (1300-1500) + HeaderSize (8) + margin.
const DefaultBufSize = 2048

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, DefaultBufSize)
		return &b
	},
}

// GetBuf returns a byte slice of the requested size from the pool.
// If the pooled buffer is too small, a fresh allocation is returned.
func GetBuf(size int) []byte {
	bp := bufPool.Get().(*[]byte)
	if cap(*bp) >= size {
		return (*bp)[:size]
	}
	// Oversized: allocate fresh, let the undersized buffer be GC'd.
	return make([]byte, size)
}

// PutBuf returns a buffer to the pool. Buffers smaller than DefaultBufSize
// are not pooled (they were allocated outside the pool).
func PutBuf(buf []byte) {
	if cap(buf) < DefaultBufSize {
		return
	}
	b := buf[:cap(buf)]
	bufPool.Put(&b)
}
