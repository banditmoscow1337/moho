//go:build linux && (amd64 || arm64) && !safem_test_noaudio

package audio

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestALSANullDuplexWithoutCGO(t *testing.T) {
	b := &linuxBackend{}
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, _, err := b.ListDevices(); err != nil {
		t.Fatalf("enumerate audio devices: %v", err)
	}
	var count atomic.Int32
	d, err := b.Open(DeviceConfig{InputID: "alsa:null", OutputID: "alsa:null", SampleRate: 48000, Channels: 1, PeriodMillis: 20}, func(out, in []byte, frames uint32) {
		if len(in) != int(frames)*4 || len(out) != len(in) {
			t.Errorf("invalid PCM buffers: %d %d %d", len(in), len(out), frames)
		}
		count.Add(1)
	})
	if err == nil {
		err = d.Start()
	}
	if err != nil {
		if os.Getenv("SAFEM_REQUIRE_NATIVE_AUDIO_TEST") == "1" {
			t.Fatal(err)
		}
		t.Skipf("ALSA null PCM unavailable: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if count.Load() == 0 {
		t.Fatal("ALSA null PCM did not drive callbacks")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestALSANullPlaybackWithoutCapture(t *testing.T) {
	b := &linuxBackend{}
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var calls atomic.Int32
	d, err := b.Open(DeviceConfig{
		InputID: "alsa:does-not-exist", OutputID: "alsa:null",
		SampleRate: 48000, Channels: 2, PeriodMillis: 20, PlaybackOnly: true,
	}, func(out, in []byte, frames uint32) {
		if in != nil || len(out) != int(frames)*8 {
			t.Errorf("playback-only PCM buffers: %d, %d", len(in), len(out))
		}
		calls.Add(1)
	})
	if err == nil {
		err = d.Start()
	}
	if err != nil {
		if os.Getenv("SAFEM_REQUIRE_NATIVE_AUDIO_TEST") == "1" {
			t.Fatal(err)
		}
		t.Skipf("ALSA null PCM unavailable: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Fatal("playback-only device did not drive callbacks")
	}
}
