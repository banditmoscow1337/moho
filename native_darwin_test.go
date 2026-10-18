//go:build darwin && (amd64 || arm64) && !safem_test_noaudio

package audio

import (
	"bytes"
	"sync"
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

func TestCoreAudioDefaultDeviceQueryDoesNotBlockPlayback(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	b := &coreAudioBackend{
		property: func(_ uint32, _ *macAddress, _ uint32, _ uintptr, _ *uint32, out uintptr) int32 {
			once.Do(func() { close(entered) })
			<-release
			*(*uint32)(unsafe.Pointer(out)) = 42
			return 0
		},
	}
	s := &macStream{
		api: b, playback: true, channels: 2, deviceID: 42, defaultScope: macDefaultOutput,
		fifo: macFIFO{data: make([]byte, 256)},
	}
	s.lastCallback.Store(time.Now().UnixNano())
	s.startDefaultMonitor()
	defer func() {
		close(release)
		s.stopDefaultMonitor()
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("default device check did not start")
	}
	finished := make(chan error, 1)
	go func() {
		_, err := s.transfer(make([]byte, 8))
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("playback transfer waited for the CoreAudio property query")
	}
}

func TestCoreAudioDefaultDeviceChangeTriggersReconnect(t *testing.T) {
	b := &coreAudioBackend{
		property: func(_ uint32, _ *macAddress, _ uint32, _ uintptr, _ *uint32, out uintptr) int32 {
			*(*uint32)(unsafe.Pointer(out)) = 99
			return 0
		},
	}
	s := &macStream{
		api: b, playback: true, channels: 2, deviceID: 42, defaultScope: macDefaultOutput,
		fifo: macFIFO{data: make([]byte, 256)},
	}
	s.lastCallback.Store(time.Now().UnixNano())
	s.startDefaultMonitor()
	defer s.stopDefaultMonitor()
	deadline := time.Now().Add(time.Second)
	for !s.failed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.failed.Load() {
		t.Fatal("default device change was not detected")
	}
	if _, err := s.transfer(make([]byte, 8)); err == nil {
		t.Fatal("playback did not request a reconnect")
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
	underruns, missing := b.outputUnderruns()
	if underruns != 1 || missing != 1 {
		t.Fatalf("CoreAudio underruns=%d missing frames=%d, want 1 and 1", underruns, missing)
	}
}
