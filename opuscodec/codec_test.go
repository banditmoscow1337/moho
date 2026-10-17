//go:build (darwin || linux || windows) && (amd64 || arm64)

package opuscodec

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingLibraryReportsRequestedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "libopus-missing")
	t.Setenv("SAFEM_OPUS_LIBRARY", path)
	if _, err := Version(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("missing runtime library must name its path: %v", err)
	}
}

func requireLibopus(t testing.TB) {
	t.Helper()
	version, err := Version()
	if err != nil {
		if os.Getenv("SAFEM_REQUIRE_OPUS_TEST") == "1" {
			t.Fatal(err)
		}
		t.Skipf("libopus not available: %v", err)
	}
	if !strings.Contains(strings.ToLower(version), "opus") {
		t.Fatalf("unexpected libopus version: %q", version)
	}
}

func TestNativeRoundTripAndPLC(t *testing.T) {
	requireLibopus(t)
	for _, format := range []struct{ rate, channels int }{{48000, 1}, {16000, 2}} {
		t.Run(fmt.Sprintf("%dHz/%dch", format.rate, format.channels), func(t *testing.T) {
			enc, err := NewEncoder(format.rate, format.channels)
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()
			dec, err := NewDecoder(format.rate, format.channels)
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()
			frame := format.rate / 50
			pcm := make([]float32, frame*format.channels)
			out := make([]float32, len(pcm))
			packet := make([]byte, 1200)
			var energy float64
			for j := range 5 {
				for i := range frame {
					v := float32(0.5 * math.Sin(2*math.Pi*440*float64(j*frame+i)/float64(format.rate)))
					for ch := range format.channels {
						pcm[i*format.channels+ch] = v
					}
				}
				n, err := enc.EncodeFloat32(pcm, packet)
				if err != nil || n < 1 || n > len(packet) {
					t.Fatalf("encode: %d, %v", n, err)
				}
				decoded, err := dec.DecodeFloat32(packet[:n], out)
				if err != nil || decoded != frame {
					t.Fatalf("decode: %d, %v", decoded, err)
				}
				for _, v := range out {
					energy += math.Abs(float64(v))
				}
			}
			if energy < 10 {
				t.Fatalf("round trip produced silence: energy %g", energy)
			}
			if n, err := dec.DecodeFloat32(nil, out); err != nil || n != frame {
				t.Fatalf("packet loss concealment: %d, %v", n, err)
			}
		})
	}
}

func TestErrorsAndLifetime(t *testing.T) {
	if _, err := NewEncoder(44100, 1); err == nil {
		t.Fatal("invalid format accepted")
	}
	requireLibopus(t)
	for range 100 {
		enc, err := NewEncoder(48000, 1)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := NewDecoder(48000, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := enc.EncodeFloat32(make([]float32, 3), make([]byte, 1200)); err == nil {
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
		if _, err := enc.EncodeFloat32(nil, nil); !errors.Is(err, ErrClosed) {
			t.Fatalf("encoder after Close: %v", err)
		}
		if _, err := dec.DecodeFloat32(nil, nil); !errors.Is(err, ErrClosed) {
			t.Fatalf("decoder after Close: %v", err)
		}
	}
}

func BenchmarkNativeRoundTrip(b *testing.B) {
	requireLibopus(b)
	enc, _ := NewEncoder(48000, 1)
	defer enc.Close()
	dec, _ := NewDecoder(48000, 1)
	defer dec.Close()
	pcm, packet, out := make([]float32, 960), make([]byte, 1200), make([]float32, 960)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		n, err := enc.EncodeFloat32(pcm, packet)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := dec.DecodeFloat32(packet[:n], out); err != nil {
			b.Fatal(err)
		}
	}
}
