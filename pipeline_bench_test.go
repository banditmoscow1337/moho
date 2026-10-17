package audio

import (
	"fmt"
	"testing"
)

func BenchmarkAudioCallback(b *testing.B) {
	for _, frames := range []int{128, 256, 480, 960, 1920} {
		b.Run(fmt.Sprintf("duplex/%d", frames), func(b *testing.B) {
			p := newAudioPipeline(NewEngineWithComponents(nil, nil))
			in, out := make([]byte, frames*4), make([]byte, frames*4)
			b.SetBytes(int64(frames * 8))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				p.playback.writeBytes(in)
				p.callback(out, in, uint32(frames))
				p.capture.readBytes(out)
			}
		})
	}
	for _, scenario := range []string{"underrun", "overflow", "timing"} {
		b.Run(scenario, func(b *testing.B) {
			p := newAudioPipeline(NewEngineWithComponents(nil, nil))
			p.timing = scenario == "timing"
			in, out := make([]byte, frameBytes), make([]byte, frameBytes)
			if scenario == "overflow" {
				p.capture.writeSilence(len(p.capture.buf))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if scenario == "timing" {
					p.playback.writeBytes(in)
				}
				p.callback(out, in, FrameSizeSamples)
				if scenario != "overflow" {
					p.capture.readBytes(out)
				}
			}
		})
	}
}
