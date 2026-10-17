package voicebuf

import "testing"

func BenchmarkVoiceBuffer(b *testing.B) {
	src := make([]byte, PacketSize)
	b.ReportAllocs()
	b.SetBytes(PacketSize)
	b.ResetTimer()
	for range b.N {
		p := Get()
		*p = append((*p)[:0], src...)
		Put(p)
	}
}

func BenchmarkVoiceBufferParallel(b *testing.B) {
	src := make([]byte, PacketSize)
	b.ReportAllocs()
	b.SetBytes(PacketSize)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p := Get()
			*p = append((*p)[:0], src...)
			Put(p)
		}
	})
}
