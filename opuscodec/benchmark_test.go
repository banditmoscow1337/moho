//go:build (darwin || linux || windows) && (amd64 || arm64)

package opuscodec

import (
	"math"
	"testing"
)

// Synthetic inputs are reproducible across machines and contain no recorded speech.
func benchPCM(kind string) []float32 {
	pcm := make([]float32, 960)
	state := uint32(1)
	for i := range pcm {
		state = 1664525*state + 1013904223
		switch kind {
		case "voiced":
			pcm[i] = float32(0.12*math.Sin(2*math.Pi*173*float64(i)/48000) + 0.04*math.Sin(2*math.Pi*691*float64(i)/48000))
		case "noise":
			pcm[i] = float32(int32(state)) / 2147483648 * 0.1
		}
	}
	return pcm
}

func BenchmarkOpusEncode(b *testing.B) {
	requireLibopus(b)
	for _, kind := range []string{"silence", "voiced", "noise"} {
		b.Run(kind, func(b *testing.B) {
			e, err := NewEncoder(48000, 1)
			if err != nil {
				b.Fatal(err)
			}
			defer e.Close()
			pcm, packet := benchPCM(kind), make([]byte, 1200)
			for range 50 {
				if _, err = e.EncodeFloat32(pcm, packet); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.SetBytes(960 * 4)
			b.ResetTimer()
			var bytes int64
			for range b.N {
				n, err := e.EncodeFloat32(pcm, packet)
				if err != nil {
					b.Fatal(err)
				}
				bytes += int64(n)
			}
			b.ReportMetric(float64(bytes)/float64(b.N), "packet-B/op")
		})
	}
}

func BenchmarkOpusDecode(b *testing.B) {
	requireLibopus(b)
	e, err := NewEncoder(48000, 1)
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	pcm, packet := benchPCM("voiced"), make([]byte, 1200)
	n, err := e.EncodeFloat32(pcm, packet)
	if err != nil {
		b.Fatal(err)
	}
	packet = packet[:n]
	for _, kind := range []string{"normal", "loss10pct", "plc"} {
		b.Run(kind, func(b *testing.B) {
			d, err := NewDecoder(48000, 1)
			if err != nil {
				b.Fatal(err)
			}
			defer d.Close()
			out := make([]float32, 960)
			if _, err = d.DecodeFloat32(packet, out); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(960 * 4)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				in := packet
				if kind == "plc" || kind == "loss10pct" && i%10 == 9 {
					in = nil
				}
				if _, err = d.DecodeFloat32(in, out); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkOpusLifecycle(b *testing.B) {
	requireLibopus(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		e, err := NewEncoder(48000, 1)
		if err != nil {
			b.Fatal(err)
		}
		d, err := NewDecoder(48000, 1)
		if err != nil {
			e.Close()
			b.Fatal(err)
		}
		e.Close()
		d.Close()
	}
}
