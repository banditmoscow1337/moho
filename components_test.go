package audio

import (
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"
)

type testBackend struct {
	config        DeviceConfig
	callback      DataCallback
	devices       []*testDevice
	closed        bool
	initErr       error
	openErrInput  DeviceID
	startErrInput DeviceID
}

func (b *testBackend) Init() error { return b.initErr }
func (b *testBackend) ListDevices() ([]DeviceInfo, []DeviceInfo, error) {
	return []DeviceInfo{{ID: "mic", Name: "Test mic"}},
		[]DeviceInfo{{ID: "speaker", Name: "Test speaker"}}, nil
}
func (b *testBackend) Open(config DeviceConfig, callback DataCallback) (Device, error) {
	b.config, b.callback = config, callback
	if config.InputID == b.openErrInput && b.openErrInput != "" {
		return nil, errors.New("input device unavailable")
	}
	d := &testDevice{}
	if config.InputID == b.startErrInput && b.startErrInput != "" {
		d.startErr = errors.New("input device failed to start")
	}
	b.devices = append(b.devices, d)
	return d, nil
}
func (b *testBackend) Close() error { b.closed = true; return nil }

type testDevice struct {
	started, stopped, closed int
	startErr                 error
}

func (d *testDevice) Start() error { d.started++; return d.startErr }
func (d *testDevice) Stop() error  { d.stopped++; return nil }
func (d *testDevice) Close() error { d.closed++; return nil }

type testCodecs struct {
	encoders   []*testEncoder
	decoders   []*testDecoder
	encoderErr error
	decoderErr error
	encodeGate <-chan struct{}
}

func (f *testCodecs) NewEncoder(rate, channels int) (Encoder, error) {
	if f.encoderErr != nil {
		return nil, f.encoderErr
	}
	if rate != SampleRate || channels != Channels {
		return nil, errors.New("unexpected format")
	}
	e := &testEncoder{gate: f.encodeGate}
	f.encoders = append(f.encoders, e)
	return e, nil
}
func (f *testCodecs) NewDecoder(rate, channels int) (Decoder, error) {
	if f.decoderErr != nil {
		return nil, f.decoderErr
	}
	d := &testDecoder{}
	f.decoders = append(f.decoders, d)
	return d, nil
}

type testEncoder struct {
	closed bool
	gate   <-chan struct{}
}

func (e *testEncoder) EncodeFloat32(pcm []float32, packet []byte) (int, error) {
	if e.gate != nil {
		<-e.gate
	}
	if len(pcm) != FrameSizeSamples {
		return 0, errors.New("unexpected frame length")
	}
	packet[0] = 0x42
	return 1, nil
}
func (e *testEncoder) Close() error { e.closed = true; return nil }

type testDecoder struct{ closed bool }

func (d *testDecoder) DecodeFloat32(packet []byte, pcm []float32) (int, error) {
	for i := range pcm[:FrameSizeSamples] {
		pcm[i] = 0.25
	}
	return FrameSizeSamples, nil
}
func (d *testDecoder) Close() error { d.closed = true; return nil }

type gatedDecoder struct {
	entered chan struct{}
	release chan struct{}
	closed  chan struct{}
}

func (d *gatedDecoder) DecodeFloat32([]byte, []float32) (int, error) {
	close(d.entered)
	<-d.release
	return FrameSizeSamples, nil
}
func (d *gatedDecoder) Close() error { close(d.closed); return nil }

func TestRemovePeerWaitsForPCMWorker(t *testing.T) {
	backend := &testBackend{}
	e := NewEngineWithComponents(backend, &testCodecs{})
	defer e.Cleanup()
	if err := e.AddPeer("peer"); err != nil {
		t.Fatal(err)
	}
	d := &gatedDecoder{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	defer func() {
		select {
		case <-d.release:
		default:
			close(d.release)
		}
	}()
	e.inputs.Load().(map[string]*JitterBuffer)["peer"].decoder = d
	if err := e.Start(nil); err != nil {
		t.Fatal(err)
	}
	for seq := uint16(0); seq < 3; seq++ {
		packet := []byte{0, byte(seq), 0x42}
		e.ProcessIncoming("peer", &packet)
	}
	backend.callback(make([]byte, frameBytes), make([]byte, frameBytes), FrameSizeSamples)
	select {
	case <-d.entered:
	case <-time.After(time.Second):
		t.Fatal("decoder was not called")
	}
	done := make(chan struct{})
	go func() { e.RemovePeer("peer"); close(done) }()
	select {
	case <-done:
		t.Fatal("peer was removed while its decoder was still active")
	case <-d.closed:
		t.Fatal("decoder was closed during a worker call")
	case <-time.After(20 * time.Millisecond):
	}
	close(d.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("peer removal did not finish")
	}
	select {
	case <-d.closed:
	default:
		t.Fatal("decoder was not closed after the worker finished")
	}
}

func TestEngineComponentsAndOwnership(t *testing.T) {
	gate := make(chan struct{})
	backend, codecs := &testBackend{}, &testCodecs{encodeGate: gate}
	engine := NewEngineWithComponents(backend, codecs)
	in, out, err := engine.ListDevices()
	if err != nil || len(in) != 1 || len(out) != 1 {
		t.Fatalf("device inventory: %v, %v, %v", in, out, err)
	}
	engine.SetInputDevice(in[0].ID)
	engine.SetOutputDevice(out[0].ID)
	if err := engine.AddPeer("peer"); err != nil {
		t.Fatal(err)
	}
	sent := make(chan []byte, 10)
	if err := engine.Start(func(packet []byte) {
		sent <- append([]byte(nil), packet...)
	}); err != nil {
		t.Fatal(err)
	}
	if backend.config.InputID != "mic" || backend.config.OutputID != "speaker" ||
		backend.config.SampleRate != SampleRate || backend.config.Channels != Channels ||
		backend.config.PeriodMillis != FrameSizeMs {
		t.Fatalf("device configuration: %+v", backend.config)
	}
	for seq := uint16(0); seq < JitterBufferTarget; seq++ {
		packet := make([]byte, 3)
		binary.BigEndian.PutUint16(packet, seq)
		packet[2] = 0x42
		engine.ProcessIncoming("peer", &packet)
	}
	input := make([]byte, FrameSizeSamples*4)
	output := make([]byte, FrameSizeSamples*4)
	backend.callback(output, input, FrameSizeSamples)
	// The codec is blocked, but the hardware callback must have returned.
	close(gate)
	select {
	case packet := <-sent:
		if len(packet) != 3 || packet[2] != 0x42 {
			t.Fatalf("outgoing packet: %v", packet)
		}
	case <-time.After(time.Second):
		t.Fatal("PCM worker did not encode")
	}
	waitPlayback(t, engine, frameBytes)
	backend.callback(output, nil, FrameSizeSamples)
	if sample := math.Float32frombits(binary.LittleEndian.Uint32(output[:4])); sample != 0.25 {
		t.Fatalf("decoded playback sample: %v", sample)
	}
	if err := engine.Restart(); err != nil {
		t.Fatal(err)
	}
	if len(backend.devices) != 2 || backend.devices[0].closed != 1 || !codecs.encoders[0].closed {
		t.Fatal("restart did not close the old device and encoder")
	}
	engine.Cleanup()
	if !backend.closed || backend.devices[1].closed != 1 ||
		!codecs.encoders[1].closed || !codecs.decoders[0].closed {
		t.Fatal("cleanup did not release owned components")
	}
}

func TestVariableHardwareBlocksAndBoundedRings(t *testing.T) {
	gate := make(chan struct{})
	backend, codecs := &testBackend{}, &testCodecs{encodeGate: gate}
	e := NewEngineWithComponents(backend, codecs)
	defer e.Cleanup()
	if err := e.AddPeer("peer"); err != nil {
		t.Fatal(err)
	}
	for seq := uint16(0); seq < JitterBufferTarget; seq++ {
		packet := []byte{byte(seq >> 8), byte(seq), 0x42}
		e.ProcessIncoming("peer", &packet)
	}
	sent := make(chan []byte, 4)
	if err := e.Start(func(packet []byte) {
		sent <- append([]byte(nil), packet...)
	}); err != nil {
		t.Fatal(err)
	}
	for _, frames := range []uint32{256, 704} {
		backend.callback(make([]byte, frames*4), make([]byte, frames*4), frames)
	}
	close(gate)
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("split capture blocks did not produce one Opus frame")
	}
	waitPlayback(t, e, frameBytes)
	for _, frames := range []uint32{256, 704} {
		out := make([]byte, frames*4)
		backend.callback(out, nil, frames)
		for i := 0; i < len(out); i += 4 {
			if v := math.Float32frombits(binary.LittleEndian.Uint32(out[i:])); v != 0.25 {
				t.Fatalf("split playback block: sample %d = %v", i/4, v)
			}
		}
	}
	// With the worker held in the codec, a very large callback must drop
	// excess capture rather than allocate or block indefinitely.
	e.Stop()
	e.Reset()
	blocked := make(chan struct{})
	codecs.encodeGate = blocked
	if err := e.Start(nil); err != nil {
		t.Fatal(err)
	}
	frames := uint32(FrameSizeSamples * 20)
	backend.callback(make([]byte, frames*4), make([]byte, frames*4), frames)
	if stats := e.Stats(); stats.CaptureDropped == 0 || stats.PlaybackSilence == 0 {
		t.Fatalf("expected bounded ring overrun and silence: %+v", stats)
	}
	close(blocked)
	deadline := time.Now().Add(time.Second)
	for e.Stats().PlaybackDropped == 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker did not drop output after the playback ring filled")
		}
		runtime.Gosched()
	}
}

func waitPlayback(t *testing.T, e *Engine, bytes int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for e.pipeline.playback.len() < bytes {
		if time.Now().After(deadline) {
			t.Fatal("PCM worker did not prepare playback")
		}
		runtime.Gosched()
	}
}

func TestCallbackDoesNotAllocate(t *testing.T) {
	p := newAudioPipeline(NewEngineWithComponents(&testBackend{}, &testCodecs{}))
	input, output := make([]byte, frameBytes), make([]byte, frameBytes)
	if n := testing.AllocsPerRun(100, func() {
		p.capture.reset()
		p.playback.reset()
		p.callback(output, input, FrameSizeSamples)
	}); n != 0 {
		t.Fatalf("hardware callback allocates: %g allocs/call", n)
	}
}

func TestCallbackPadsMissingPCMAndShortPlayback(t *testing.T) {
	p := newAudioPipeline(NewEngineWithComponents(&testBackend{}, &testCodecs{}))
	input := make([]byte, 4)
	binary.LittleEndian.PutUint32(input, math.Float32bits(0.5))
	out := make([]byte, 12)
	p.callback(out, input, 4)
	var captured [16]byte
	if n := p.capture.readBytes(captured[:]); n != 16 {
		t.Fatalf("captured %d bytes, want 16", n)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(captured[:])); got != 0.5 {
		t.Fatalf("captured input: %v", got)
	}
	for _, b := range captured[4:] {
		if b != 0 {
			t.Fatal("missing microphone samples were not padded with silence")
		}
	}
	ready := make([]byte, 8)
	binary.LittleEndian.PutUint32(ready[:4], math.Float32bits(0.25))
	binary.LittleEndian.PutUint32(ready[4:], math.Float32bits(-0.25))
	p.playback.writeBytes(ready)
	p.callback(out, nil, 4)
	if a, b := math.Float32frombits(binary.LittleEndian.Uint32(out[:4])), math.Float32frombits(binary.LittleEndian.Uint32(out[4:8])); a != 0.25 || b != -0.25 {
		t.Fatalf("playback samples: %v, %v", a, b)
	}
	if binary.LittleEndian.Uint32(out[8:]) != 0 {
		t.Fatal("playback underrun was not padded with silence")
	}
}

func TestConcurrentPCMIngress(t *testing.T) {
	r := newPCMRing(128)
	const count = 1000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range count {
			var sample [4]byte
			binary.LittleEndian.PutUint32(sample[:], uint32(i))
			for r.writeBytes(sample[:]) != 4 {
				// The consumer is catching up; the producer cannot overwrite it.
				runtime.Gosched()
			}
		}
	}()
	var sample [4]byte
	for i := range count {
		for r.readBytes(sample[:]) != 4 {
			runtime.Gosched()
		}
		if got := binary.LittleEndian.Uint32(sample[:]); got != uint32(i) {
			t.Fatalf("ring sample %d: got %d", i, got)
		}
	}
	wg.Wait()
}

func TestEngineComponentErrors(t *testing.T) {
	backend := &testBackend{initErr: errors.New("no backend")}
	codecs := &testCodecs{}
	engine := NewEngineWithComponents(backend, codecs)
	if err := engine.Init(); err == nil || len(codecs.encoders) != 1 || !codecs.encoders[0].closed {
		t.Fatalf("initialization leak or missing error: %v", err)
	}
	codecs.decoderErr = errors.New("decoder unavailable")
	if err := engine.AddPeer("peer"); err == nil {
		t.Fatal("decoder creation failure was ignored")
	}
	if len(engine.inputs.Load().(map[string]*JitterBuffer)) != 0 {
		t.Fatal("failed peer was published")
	}
}

func TestIdleDeviceSelectionReleasesPreparedDevice(t *testing.T) {
	backend, codecs := &testBackend{}, &testCodecs{}
	e := NewEngineWithComponents(backend, codecs)
	defer e.Cleanup()
	if err := e.Init(); err != nil {
		t.Fatal(err)
	}
	if err := e.SelectDevices("new-mic", "new-speaker"); err != nil {
		t.Fatal(err)
	}
	if backend.devices[0].closed != 1 || !codecs.encoders[0].closed {
		t.Fatal("idle device selection retained the old native resources")
	}
	if input, output := e.SelectedDevices(); input != "new-mic" || output != "new-speaker" {
		t.Fatalf("selected devices: %q, %q", input, output)
	}
	if err := e.Start(nil); err != nil {
		t.Fatal(err)
	}
	if backend.config.InputID != "new-mic" || backend.config.OutputID != "new-speaker" {
		t.Fatalf("next call opened stale devices: %+v", backend.config)
	}
	e.Stop()
	if err := e.Restart(); err != nil {
		t.Fatal(err)
	}
	if backend.devices[1].closed != 1 || !codecs.encoders[1].closed || len(backend.devices) != 2 {
		t.Fatal("idle restart did not release prepared resources")
	}
}

func TestActiveDeviceSwitchRestoresCallAfterFailure(t *testing.T) {
	backend := &testBackend{openErrInput: "unavailable", startErrInput: "broken"}
	e := NewEngineWithComponents(backend, &testCodecs{})
	defer e.Cleanup()
	if err := e.Start(nil); err != nil {
		t.Fatal(err)
	}
	if err := e.SelectDevices("next", "speaker"); err != nil {
		t.Fatal(err)
	}
	if !e.running || backend.config.InputID != "next" || backend.devices[0].closed != 1 {
		t.Fatal("active device switch did not restart the call")
	}
	for _, failed := range []DeviceID{"unavailable", "broken"} {
		if err := e.SelectDevices(failed, "speaker"); err == nil {
			t.Fatalf("selection of %s unexpectedly succeeded", failed)
		}
		if input, output := e.SelectedDevices(); input != "next" || output != "speaker" || !e.running {
			t.Fatalf("failed switch lost the call or selection: %q, %q", input, output)
		}
	}
	for i, device := range backend.devices {
		if device.closed != 1 && i != len(backend.devices)-1 {
			t.Fatalf("device %d leaked after switch: %+v", i, device)
		}
	}
	if backend.devices[len(backend.devices)-1].started != 1 {
		t.Fatal("restored call did not start its device")
	}
}

func TestRepeatedCallsCloseEachDeviceAndCodec(t *testing.T) {
	backend, codecs := &testBackend{}, &testCodecs{}
	e := NewEngineWithComponents(backend, codecs)
	for range 30 {
		if err := e.Start(nil); err != nil {
			t.Fatal(err)
		}
		e.Stop()
		e.Reset()
	}
	e.Cleanup()
	e.Cleanup()
	if !backend.closed || len(backend.devices) != 30 || len(codecs.encoders) != 30 {
		t.Fatalf("cycle counts: devices=%d encoders=%d backend closed=%t", len(backend.devices), len(codecs.encoders), backend.closed)
	}
	for i := range backend.devices {
		if backend.devices[i].closed != 1 || backend.devices[i].stopped != 1 || !codecs.encoders[i].closed {
			t.Fatalf("cycle %d retained native state", i)
		}
	}
}

func TestMissingCodecDoesNotOpenCaptureDevice(t *testing.T) {
	backend := &testBackend{}
	e := NewEngineWithComponents(backend, &testCodecs{encoderErr: errors.New("load libopus from /missing/libopus.so: no such file")})
	defer e.Cleanup()
	if err := e.Start(nil); err == nil || !errors.Is(err, e.codecs.(*testCodecs).encoderErr) {
		t.Fatalf("missing runtime library error was lost: %v", err)
	}
	if len(backend.devices) != 0 {
		t.Fatal("capture device opened without an audio codec")
	}
}
