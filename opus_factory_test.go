//go:build !safem_test_noaudio

package audio

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestOpusRoundTripAndPLC(t *testing.T) {
	factory := newDefaultCodecFactory()
	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprintf("48000Hz/%dch", channels), func(t *testing.T) {
			enc, err := factory.NewEncoder(SampleRate, channels)
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()

			dec, err := factory.NewDecoder(SampleRate, channels)
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()

			pcm := make([]float32, FrameSizeSamples*channels)
			out := make([]float32, len(pcm))
			packet := make([]byte, MaxEncodedSize)
			var energy float64
			for j := range 5 {
				for i := range FrameSizeSamples {
					v := float32(0.5 * math.Sin(2*math.Pi*440*float64(j*FrameSizeSamples+i)/ float64(SampleRate)))
					for ch := range channels {
						pcm[i*channels+ch] = v
					}
				}
				n, err := enc.EncodeFloat32(pcm, packet)
				if err != nil || n < 1 || n > len(packet) {
					t.Fatalf("encode: %d, %v", n, err)
				}
				decoded, err := dec.DecodeFloat32(packet[:n], out)
				if err != nil || decoded != FrameSizeSamples {
					t.Fatalf("decode: %d, %v", decoded, err)
				}
				for _, v := range out {
					energy += math.Abs(float64(v))
				}
			}
			if energy < 10 {
				t.Fatalf("round trip produced silence: energy %g", energy)
			}
			if n, err := dec.DecodeFloat32(nil, out); err != nil || n != FrameSizeSamples {
				t.Fatalf("packet loss concealment: %d, %v", n, err)
			}
		})
	}
}

func TestOpusErrorsAndLifetime(t *testing.T) {
	factory := newDefaultCodecFactory()
	if _, err := factory.NewEncoder(44100, 1); err == nil {
		t.Fatal("invalid encoder sample rate accepted")
	}
	if _, err := factory.NewDecoder(44100, 1); err == nil {
		t.Fatal("invalid decoder sample rate accepted")
	}

	enc, err := factory.NewEncoder(SampleRate, Channels)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := factory.NewDecoder(SampleRate, Channels)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := enc.EncodeFloat32(make([]float32, 3), make([]byte, MaxEncodedSize)); err == nil {
		t.Fatal("invalid frame size accepted")
	}
	if _, err := dec.DecodeFloat32(nil, nil); err == nil {
		t.Fatal("empty output buffer accepted")
	}

	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dec.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dec.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := enc.EncodeFloat32(make([]float32, FrameSizeSamples), make([]byte, MaxEncodedSize)); !errors.Is(err, errCodecClosed) {
		t.Fatalf("encoder after Close: %v", err)
	}
	if _, err := dec.DecodeFloat32(nil, make([]float32, FrameSizeSamples)); !errors.Is(err, errCodecClosed) {
		t.Fatalf("decoder after Close: %v", err)
	}
}