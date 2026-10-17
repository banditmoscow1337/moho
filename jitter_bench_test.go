package audio

import "testing"

var benchmarkJitterBuffer *JitterBuffer

// Benchmarks isolate the jitter buffer's per-peer footprint and steady-state
// packet bookkeeping from network I/O and codec work.
func BenchmarkJitterPeerLifecycle(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		jb := NewJitterBuffer(JitterBufferTarget)
		jb.Cleanup()
		benchmarkJitterBuffer = jb
	}
}

func BenchmarkJitterSteady(b *testing.B) {
	jb := NewJitterBuffer(JitterBufferTarget)
	defer jb.Cleanup()
	for seq := uint16(0); seq < JitterBufferTarget; seq++ {
		p := new([]byte)
		*p = make([]byte, MaxEncodedSize+2)
		jb.Push(seq, p)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := jb.Pop()
		if p == nil {
			b.Fatal("jitter unexpectedly buffering")
		}
		jb.Push(uint16(i+JitterBufferTarget), p)
	}
}

// Includes pooled packet ownership, startup reordering, duplicate rejection,
// a missing frame and sequence wrap. One operation is four playout frames.
func BenchmarkJitterImpaired(b *testing.B) {
	jb := NewJitterBuffer(3)
	defer jb.Cleanup()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seq := uint16(i * 4)
		pushTestPacket(jb, seq+1)
		pushTestPacket(jb, seq)
		pushTestPacket(jb, seq+1)
		pushTestPacket(jb, seq+3)
		for range 4 {
			if p := jb.Pop(); p != nil {
				releasePacket(p)
			}
		}
	}
}
