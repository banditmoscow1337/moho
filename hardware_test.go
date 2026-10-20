//go:build (darwin || linux || windows) && (amd64 || arm64) && !safem_test_noaudio

package moho

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestHardwareCallCycles is opt-in because hosted CI runners have no physical
// microphone or speaker. It requires the same runtime libraries as the CLI.
func TestHardwareCallCycles(t *testing.T) {
	if os.Getenv("SAFEM_AUDIO_HARDWARE") != "1" {
		t.Skip("set SAFEM_AUDIO_HARDWARE=1 on a machine with an audio input and output")
	}
	input, output := DeviceID(os.Getenv("SAFEM_AUDIO_INPUT_ID")), DeviceID(os.Getenv("SAFEM_AUDIO_OUTPUT_ID"))
	backend := newDefaultBackend()
	if err := backend.Init(); err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if _, _, err := backend.ListDevices(); err != nil {
		t.Fatal(err)
	}

	var callbacks atomic.Uint64
	device, err := backend.Open(DeviceConfig{
		InputID: input, OutputID: output, SampleRate: SampleRate,
		Channels: Channels, PeriodMillis: FrameSizeMs,
	}, func(out, in []byte, frames uint32) {
		if len(in) == int(frames)*4 && len(out) == len(in) {
			callbacks.Add(1)
		}
		clear(out)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := device.Start(); err != nil {
		_ = device.Close()
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := device.Close(); err != nil {
		t.Fatal(err)
	}
	if callbacks.Load() == 0 {
		t.Fatal("native audio callback was not driven by the selected hardware")
	}

	engine := NewEngine()
	defer engine.Cleanup()
	if err := engine.SelectDevices(input, output); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := engine.Start(nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		if err := engine.SelectDevices(input, output); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		engine.Stop()
		engine.Reset()
	}
	t.Logf("%d native callbacks; three call cycles and active device restarts succeeded", callbacks.Load())
}
