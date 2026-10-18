//go:build !safem_test_noaudio

package audio

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestCaptureQueueBoundsDrift(t *testing.T) {
	q := captureQueue{data: make([]byte, 16)}
	for n := byte(1); n <= 6; n++ {
		q.push([]byte{n, n, n, n})
	}
	buf := make([]byte, 4)
	q.pop(buf)
	if buf[0] != 5 {
		t.Fatalf("oldest retained byte = %d, want 5", buf[0])
	}
	q.pop(buf)
	if buf[0] != 6 {
		t.Fatalf("next retained byte = %d, want 6", buf[0])
	}
}

type mockNativeBackend struct {
	opens  atomic.Int32
	writes chan struct{}
}

func (b *mockNativeBackend) Init() error                                      { return nil }
func (b *mockNativeBackend) ListDevices() ([]DeviceInfo, []DeviceInfo, error) { return nil, nil, nil }
func (b *mockNativeBackend) Open(c DeviceConfig, cb DataCallback) (Device, error) {
	return openNativeDevice(b, c, cb)
}
func (b *mockNativeBackend) Close() error { return nil }
func (b *mockNativeBackend) openStreams(c DeviceConfig) (pcmStream, pcmStream, error) {
	if c.PlaybackOnly {
		b.opens.Add(1)
		return nil, &mockNativeStream{playback: true, writes: b.writes}, nil
	}
	first := b.opens.Add(1) == 1
	return &mockNativeStream{}, &mockNativeStream{playback: true, fail: first}, nil
}

type mockNativeStream struct {
	playback, fail bool
	writes         chan struct{}
}

func (s *mockNativeStream) transfer(buf []byte) (int, error) {
	if s.fail {
		s.fail = false
		return 0, errors.New("device disconnected")
	}
	if !s.playback {
		clear(buf)
	} else if s.writes != nil {
		select {
		case s.writes <- struct{}{}:
		default:
		}
	}
	return len(buf), nil
}
func (*mockNativeStream) close() error { return nil }

func TestNativeDeviceReconnectAndQuiesce(t *testing.T) {
	b := &mockNativeBackend{}
	var calls atomic.Int32
	d, err := b.Open(DeviceConfig{SampleRate: 48000, Channels: 1, PeriodMillis: 20}, func(_, _ []byte, _ uint32) { calls.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for b.opens.Load() < 2 || calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("reopen=%d callback=%d", b.opens.Load(), calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	reconnects, openFailures, reason := d.(*nativeDevice).reconnectStats()
	if reconnects != 1 || openFailures != 0 || reason != "playback transfer: device disconnected" {
		t.Fatalf("reconnects=%d open failures=%d reason=%q", reconnects, openFailures, reason)
	}
	stopped := calls.Load()
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != stopped {
		t.Fatal("callback continued after Stop")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativePlaybackOnly(t *testing.T) {
	b := &mockNativeBackend{}
	called := make(chan struct{}, 1)
	d, err := b.Open(DeviceConfig{
		SampleRate: 48000, Channels: 2, PeriodMillis: 20, PlaybackOnly: true,
	}, func(out, in []byte, frames uint32) {
		if in != nil || len(out) != int(frames)*8 {
			t.Errorf("playback-only buffers: %d input bytes, %d output bytes", len(in), len(out))
		}
		select {
		case called <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("playback-only callback was not called")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativePlaybackPrimesBuffer(t *testing.T) {
	// A virtual stream accepts every write immediately. The first three
	// periods must be submitted before a full period of wall time elapses;
	// otherwise CoreAudio's FIFO starts with only one period of headroom.
	b := &mockNativeBackend{writes: make(chan struct{}, 4)}
	d, err := b.Open(DeviceConfig{
		SampleRate: 48000, Channels: 2, PeriodMillis: 500, PlaybackOnly: true,
	}, func(_, _ []byte, _ uint32) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i := 0; i < 3; i++ {
		select {
		case <-b.writes:
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("only %d periods submitted before playback pacing", i)
		}
	}
}
