// Package voicebuf owns the pooled wire buffers shared by P2P ingress,
// the audio jitter buffer and P2P egress.
package voicebuf

import "sync"

const (
	MaxEncodedSize = 1200
	// PacketSize includes the two-byte sequence header. Retaining only these
	// small buffers keeps a malformed or oversized packet out of the pool.
	PacketSize = MaxEncodedSize + 2
)

var pool = sync.Pool{New: func() any {
	b := make([]byte, 0, PacketSize)
	return &b
}}

func Get() *[]byte { return pool.Get().(*[]byte) }

// Put accepts the original base slice, including its sequence header. A
// payload subslice has smaller capacity and is intentionally never retained.
func Put(b *[]byte) {
	if b != nil && cap(*b) == PacketSize && len(*b) <= PacketSize {
		*b = (*b)[:0]
		pool.Put(b)
	}
}
