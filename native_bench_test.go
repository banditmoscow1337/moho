//go:build !safem_test_noaudio

package moho

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/banditmoscow1337/moho/voicebuf"
)

// BenchmarkAudioFrame measures the production encode/decode/mix function,
// plus pooled ingress and output-ring drain, without a device or scheduler.
// One operation handles 20 ms of capture and one frame for each remote peer.
func BenchmarkAudioFrame(b *testing.B) {
	for _, peers := range []int{1, 4, 8} {
		for _, mode := range []string{"call", "mute", "loss10pct"} {
			b.Run(fmt.Sprintf("peers%d/%s", peers, mode), func(b *testing.B) {
				codecs := newDefaultCodecFactory()
				e := NewEngineWithComponents(benchBackend{}, codecs)
				defer e.Cleanup()
				if err := e.Init(); err != nil {
					b.Fatal(err)
				}
				ids := make([]string, peers)
				for i := range ids {
					ids[i] = fmt.Sprint(i)
					if err := e.AddPeer(ids[i]); err != nil {
						b.Fatal(err)
					}
				}
				e.SetMute(mode == "mute")

				w := newPCMWorker()
				for i := range FrameSizeSamples {
					binary.LittleEndian.PutUint32(w.input[i*4:], math.Float32bits(float32(0.05*math.Sin(float64(i)*0.0576))))
				}
				remote, err := codecs.NewEncoder(SampleRate, Channels)
				if err != nil {
					b.Fatal(err)
				}
				defer remote.Close()
				pcm := make([]float32, FrameSizeSamples)
				for i := range pcm {
					pcm[i] = math.Float32frombits(binary.LittleEndian.Uint32(w.input[i*4:]))
				}
				packet := make([]byte, MaxEncodedSize)
				n, err := remote.EncodeFloat32(pcm, packet)
				if err != nil {
					b.Fatal(err)
				}

				push := func(seq uint16) {
					for _, id := range ids {
						p := voicebuf.Get()
						*p = (*p)[:n+2]
						binary.BigEndian.PutUint16((*p)[:2], seq)
						copy((*p)[2:], packet[:n])
						e.ProcessIncoming(id, p)
					}
				}
				for seq := uint16(0); seq < JitterBufferTarget; seq++ {
					push(seq)
				}

				out := make([]byte, frameBytes)
				send := func(data []byte) { p := voicebuf.Get(); *p = append((*p)[:0], data...); voicebuf.Put(p) }
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if mode != "loss10pct" || i%10 != 9 {
						push(uint16(i + JitterBufferTarget))
					}
					e.pipeline.processFrame(w, send)
					e.pipeline.playback.readBytes(out)
				}
				b.StopTimer()
				b.ReportMetric(20, "audio-ms/op")
				if stats := e.Stats(); stats.EncodeErrors != 0 || stats.DecodeErrors != 0 {
					b.Fatalf("codec errors: %+v", stats)
				}
			})
		}
	}
}

// This backend deliberately has no hardware and retains no lifecycle history.
type benchBackend struct{}
type benchDevice struct{}

func (benchBackend) Init() error                                      { return nil }
func (benchBackend) ListDevices() ([]DeviceInfo, []DeviceInfo, error) { return nil, nil, nil }
func (benchBackend) Open(DeviceConfig, DataCallback) (Device, error)  { return benchDevice{}, nil }
func (benchBackend) Close() error                                     { return nil }
func (benchDevice) Start() error                                      { return nil }
func (benchDevice) Stop() error                                       { return nil }
func (benchDevice) Close() error                                      { return nil }

func BenchmarkAudioCallLifecycle(b *testing.B) {
	for _, peers := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("peers%d", peers), func(b *testing.B) {
			ids := make([]string, peers)
			for i := range ids {
				ids[i] = fmt.Sprint(i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				e := NewEngineWithComponents(benchBackend{}, newDefaultCodecFactory())
				for _, id := range ids {
					if err := e.AddPeer(id); err != nil {
						b.Fatal(err)
					}
				}
				if err := e.Start(nil); err != nil {
					b.Fatal(err)
				}
				e.Cleanup()
			}
		})
	}
}