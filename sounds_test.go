package moho

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testSound(name string, pcm []byte) *Sound {
	return &Sound{
		Name: name, Format: PCMFormat{Channels: 1, BitsPerSample: 16, SampleRate: soundOutputRate},
		PCM: pcm, BaseVolume: 1,
	}
}

func renderSound(b *testBackend, frames uint32) []float32 {
	output := make([]byte, int(frames)*8)
	b.callback(output, nil, frames)
	samples := make([]float32, len(output)/4)
	for i := range samples {
		samples[i] = math.Float32frombits(binary.LittleEndian.Uint32(output[i*4:]))
	}
	return samples
}

func waitForVoice(t *testing.T, d *Driver, id VoiceID, ready func(*soundVoice) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		ok := false
		for i := range d.voices {
			if d.voices[i].id == id && ready(&d.voices[i]) {
				ok = true
				break
			}
		}
		d.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream did not become ready")
}

func waitForDriverError(t *testing.T, d *Driver) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := d.Update(); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream error was not reported")
	return nil
}

func TestSoundDriverPriorityAndMusicProtection(t *testing.T) {
	b := &testBackend{}
	d, err := NewDriverWithBackend(2, b)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !b.config.PlaybackOnly || b.config.Channels != 2 || b.config.SampleRate != soundOutputRate {
		t.Fatalf("playback config: %+v", b.config)
	}
	music := testSound("music", []byte{0, 64})
	music.Category = "music"
	fx := testSound("fx", []byte{0, 32})
	musicID, err := d.Play(music, 1, true)
	if err != nil || musicID == 0 {
		t.Fatalf("music: %v, %v", musicID, err)
	}
	if same, err := d.Play(music, 10, true); err != nil || same != musicID {
		t.Fatalf("looping voice was duplicated: %v, %v", same, err)
	}
	fxID, err := d.Play(fx, 2, true)
	if err != nil || fxID == 0 {
		t.Fatalf("effect: %v, %v", fxID, err)
	}
	quiet := testSound("quiet", []byte{0, 16})
	if id, err := d.Play(quiet, 1, false); err != nil || id != 0 {
		t.Fatalf("lower-priority request: %v, %v", id, err)
	}
	loud := testSound("loud", []byte{0, 16})
	if id, err := d.Play(loud, 2, true); err != nil || id == 0 || id == fxID {
		t.Fatalf("equal priority should evict the oldest eligible voice: %v, %v", id, err)
	}
	if samples := renderSound(b, 1); samples[0] != 0.625 || samples[1] != 0.625 {
		t.Fatalf("mixed voices: %v", samples)
	}
	d.StopSound("music")
	if samples := renderSound(b, 1); samples[0] != 0.125 {
		t.Fatalf("stop by sound: %v", samples)
	}
	d.StopAll()
	if samples := renderSound(b, 1); samples[0] != 0 {
		t.Fatalf("stop all: %v", samples)
	}
}

func TestSoundDriverPauseGainAndCompletion(t *testing.T) {
	b := &testBackend{}
	d, err := NewDriverWithBackend(1, b)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := testSound("beep", []byte{0, 64, 0, 64})
	if err := d.Preload(s); err != nil {
		t.Fatal(err)
	}
	// Preload owns its data even if the caller later reuses the slice.
	s.PCM[1] = 0
	id, err := d.Play(s, 1, false)
	if err != nil || id == 0 {
		t.Fatalf("play: %v, %v", id, err)
	}
	d.PauseAll()
	if sample := renderSound(b, 1)[0]; sample != 0 {
		t.Fatalf("pause advanced playback: %v", sample)
	}
	if err := d.SetMasterGain(0.5); err != nil {
		t.Fatal(err)
	}
	d.ResumeAll()
	if sample := renderSound(b, 1)[0]; sample != 0.25 {
		t.Fatalf("resumed gain: %v", sample)
	}
	d.PauseAll()
	if same, err := d.PlayDuringPause(s, 1, true); err != nil || same == 0 {
		t.Fatalf("play during pause: %v, %v", same, err)
	}
	if sample := renderSound(b, 1)[0]; sample != 0.25 {
		t.Fatalf("play during pause sample: %v", sample)
	}
	d.ResumeAll()
	d.StopVoice(id)
	if err := d.Update(); err != nil {
		t.Fatal(err)
	}
	if err := d.SetMasterGain(math.NaN()); err == nil {
		t.Fatal("NaN gain accepted")
	}
	if err := d.SetMasterGain(1.1); err == nil {
		t.Fatal("out-of-range gain accepted")
	}
	if b.closed {
		t.Fatal("backend closed before Driver.Close")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if !b.closed || b.devices[0].closed != 1 {
		t.Fatal("driver did not close its backend and device")
	}
}

func TestSoundDriverConversionAndStreaming(t *testing.T) {
	b := &testBackend{}
	d, err := NewDriverWithBackend(2, b)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := &Sound{
		Name: "stereo8", Format: PCMFormat{Channels: 2, BitsPerSample: 8, SampleRate: 24000},
		PCM: []byte{0, 255, 128, 128}, BaseVolume: 1,
	}
	if _, err := d.Play(s, 1, false); err != nil {
		t.Fatal(err)
	}
	got := renderSound(b, 3)
	if got[0] != -1 || math.Abs(float64(got[1]-0.9921875)) > 0.00001 ||
		got[2] != -0.5 || got[3] != 0.49609375 || got[4] != 0 || got[5] != 0 {
		t.Fatalf("resampled stereo PCM: %v", got)
	}
	if err := d.Update(); err != nil {
		t.Fatal(err)
	}
	var closed atomic.Int32
	stream := &Sound{
		Name: "stream", Format: PCMFormat{Channels: 1, BitsPerSample: 16, SampleRate: 48000},
		Streaming: true, BaseVolume: 1,
		PCMOpener: func() (io.ReadCloser, error) {
			return &countingReader{Reader: bytes.NewReader([]byte{0, 64, 0, 32}), closed: &closed}, nil
		},
	}
	id, err := d.Play(stream, 1, true)
	if err != nil || id == 0 {
		t.Fatalf("stream: %v, %v", id, err)
	}
	waitForVoice(t, d, id, func(v *soundVoice) bool { return !v.streaming && len(v.pcm) > 0 })
	d.workers.Wait()
	if samples := renderSound(b, 5); samples[0] != 0.5 || samples[2] != 0.25 ||
		samples[4] != 0.5 || samples[8] != 0.5 {
		t.Fatalf("looping stream: %v", samples)
	}
	if closed.Load() != 1 {
		t.Fatalf("short looping stream was not cached and closed: %d", closed.Load())
	}
	d.StopVoice(id)
	if err := d.Update(); err != nil {
		t.Fatal(err)
	}
}

type countingReader struct {
	io.Reader
	closed *atomic.Int32
}

func (r *countingReader) Close() error { r.closed.Add(1); return nil }

func TestSoundDriverErrors(t *testing.T) {
	if _, err := NewDriverWithBackend(0, &testBackend{}); err == nil {
		t.Fatal("invalid voice count accepted")
	}
	b := &testBackend{}
	d, err := NewDriverWithBackend(1, b)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	bad := testSound("bad", []byte{1})
	if err := d.Preload(bad); err == nil {
		t.Fatal("partial frame accepted")
	}
	bad.Streaming = true
	if _, err := d.Play(bad, 1, false); err == nil {
		t.Fatal("stream without opener accepted")
	}
	bad.PCMOpener = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(nil)), nil }
	if _, err := d.Play(bad, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := waitForDriverError(t, d); err == nil {
		t.Fatal("empty stream was not reported")
	}
	readErr := errors.New("bad stream")
	bad.PCMOpener = func() (io.ReadCloser, error) { return nil, readErr }
	if _, err := d.Play(bad, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := waitForDriverError(t, d); !errors.Is(err, readErr) {
		t.Fatalf("missing opener error: %v", err)
	}
	if _, err := d.Play(testSound("x", []byte{0, 0}), math.NaN(), false); err == nil {
		t.Fatal("NaN priority accepted")
	}
}

func TestSoundDriverStreamCompletionReleasesVoice(t *testing.T) {
	b := &testBackend{}
	d, err := NewDriverWithBackend(1, b)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var closed atomic.Int32
	s := &Sound{
		Name: "one-shot", Format: PCMFormat{Channels: 1, BitsPerSample: 16, SampleRate: 48000},
		Streaming: true, BaseVolume: 1,
		PCMOpener: func() (io.ReadCloser, error) {
			return &countingReader{Reader: bytes.NewReader([]byte{0, 64}), closed: &closed}, nil
		},
	}
	first, err := d.Play(s, 1, false)
	if err != nil || first == 0 {
		t.Fatalf("play stream: %v, %v", first, err)
	}
	waitForVoice(t, d, first, func(v *soundVoice) bool { return v.streamEOF && len(v.pcm) > 0 })
	d.workers.Wait()
	if sample := renderSound(b, 1)[0]; sample != 0.5 {
		t.Fatalf("stream sample: %v", sample)
	}
	if err := d.Update(); err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 1 {
		t.Fatalf("completed stream still open: %d", closed.Load())
	}
	next, err := d.Play(testSound("next", []byte{0, 32}), 1, false)
	if err != nil || next == 0 || next == first {
		t.Fatalf("voice slot was not reusable: %v, %v", next, err)
	}
}

type blockingPCMReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingPCMReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, io.EOF
}

func (r *blockingPCMReader) Close() error { return nil }

type gatedPCMReader struct {
	io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *gatedPCMReader) Read(buf []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.Reader.Read(buf)
}

func (r *gatedPCMReader) Close() error { return nil }

func TestSoundDriverReportsStreamStarvation(t *testing.T) {
	b := &testBackend{}
	d, err := NewDriverWithBackend(1, b)
	if err != nil {
		t.Fatal(err)
	}
	reader := &gatedPCMReader{
		Reader:  bytes.NewReader(bytes.Repeat([]byte{0, 64}, soundChunk)),
		started: make(chan struct{}), release: make(chan struct{}),
	}
	var unblock sync.Once
	defer func() {
		unblock.Do(func() { close(reader.release) })
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	s := &Sound{
		Name: "gated", Format: PCMFormat{Channels: 1, BitsPerSample: 16, SampleRate: soundOutputRate},
		Streaming: true, BaseVolume: 1,
		PCMOpener: func() (io.ReadCloser, error) { return reader, nil },
	}
	id, err := d.Play(s, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("stream reader did not start")
	}
	if sample := renderSound(b, 1)[0]; sample != 0 {
		t.Fatalf("stream without PCM played %v, want silence", sample)
	}
	if got := d.Diagnostics().StreamStarves; got != 1 {
		t.Fatalf("stream starvation count = %d, want 1", got)
	}
	if stats := d.Diagnostics(); stats.MixerCallbacks != 1 || stats.NonzeroMixerCallbacks != 0 {
		t.Fatalf("unexpected mixer diagnostics before refill: %+v", stats)
	}
	unblock.Do(func() { close(reader.release) })
	waitForVoice(t, d, id, func(v *soundVoice) bool { return len(v.pcm) >= soundChunk })
	if sample := renderSound(b, 1)[0]; sample != 0.5 {
		t.Fatalf("stream after refill played %v, want 0.5", sample)
	}
	if got := d.Diagnostics().StreamStarves; got != 1 {
		t.Fatalf("stream starvation grew after refill: %d", got)
	}
	if stats := d.Diagnostics(); stats.MixerCallbacks != 2 || stats.NonzeroMixerCallbacks != 1 {
		t.Fatalf("unexpected mixer diagnostics after refill: %+v", stats)
	}
}

func TestSlowStreamDoesNotBlockUpdateOrEffects(t *testing.T) {
	b := &testBackend{}
	d, err := NewDriverWithBackend(1, b)
	if err != nil {
		t.Fatal(err)
	}
	reader := &blockingPCMReader{started: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		close(reader.release)
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	stream := &Sound{
		Name: "slow", Format: PCMFormat{Channels: 1, BitsPerSample: 16, SampleRate: 48000},
		Streaming: true, BaseVolume: 1,
		PCMOpener: func() (io.ReadCloser, error) { return reader, nil },
	}
	played := make(chan error, 1)
	go func() { _, err := d.Play(stream, 1, false); played <- err }()
	select {
	case err := <-played:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Play waited for the stream reader")
	}
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("stream worker did not start")
	}
	finished := make(chan error, 1)
	go func() {
		if err := d.Update(); err != nil {
			finished <- err
			return
		}
		d.StopSound("slow")
		_, err := d.Play(testSound("effect", []byte{0, 64}), 2, false)
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("game-thread audio control waited for the stream reader")
	}
	if sample := renderSound(b, 1)[0]; sample != 0.5 {
		t.Fatalf("effect was not mixed while the stream was blocked: %v", sample)
	}
}
