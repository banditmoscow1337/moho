//go:build darwin && (amd64 || arm64) && !safem_test_noaudio

package moho

import (
	"bytes"
	"testing"
	"time"
	"unsafe"
)

func TestNativeInventory(t *testing.T) {
	b := &coreAudioBackend{}
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, _, err := b.ListDevices(); err != nil {
		t.Fatal(err)
	}
}

func TestCoreAudioPlaybackTransferSkipsDeviceProperty(t *testing.T) {
	queries := 0
	b := &coreAudioBackend{
		property: func(_ uint32, _ *macAddress, _ uint32, _ uintptr, _ *uint32, _ uintptr) int32 {
			queries++
			return 0
		},
	}
	s := &macStream{
		api: b, playback: true, channels: 2,
		fifo: macFIFO{data: make([]byte, 256)},
	}
	s.lastCallback.Store(time.Now().UnixNano())
	for i := 0; i < 32; i++ {
		if n, err := s.transfer(make([]byte, 8)); n != 8 || err != nil {
			t.Fatalf("transfer %d: n=%d err=%v", i, n, err)
		}
	}
	if queries != 0 {
		t.Fatalf("playback transfer queried device %d times", queries)
	}
	// The callback-liveness check still requests a reconnect if the
	// previous output device stops producing callbacks.
	s.lastCallback.Store(time.Now().Add(-3 * time.Second).UnixNano())
	if _, err := s.transfer(make([]byte, 8)); err == nil {
		t.Fatal("stopped CoreAudio callback did not request a reconnect")
	}
}

func TestCoreAudioOutputUnderrunCountsMissingFrames(t *testing.T) {
	b := &coreAudioBackend{}
	s := &macStream{api: b, playback: true, channels: 2, fifo: macFIFO{data: make([]byte, 32)}}
	first := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	s.fifo.write(first)
	out := bytes.Repeat([]byte{255}, 16)
	list := macBufferList{Count: 1, Buffer: macBuffer{Channels: 2, Size: uint32(len(out)), Data: unsafe.Pointer(&out[0])}}
	s.renderOutput(2, unsafe.Pointer(&list))
	if !bytes.Equal(out[:8], first) || !bytes.Equal(out[8:], make([]byte, 8)) {
		t.Fatalf("rendered output = %v", out)
	}
	callbacks, underruns, missing := b.outputStats()
	if callbacks != 1 || underruns != 1 || missing != 1 {
		t.Fatalf("CoreAudio callbacks=%d underruns=%d missing frames=%d, want 1 each", callbacks, underruns, missing)
	}
}
