//go:build linux && (amd64 || arm64) && !safem_test_noaudio

package moho

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

func newDefaultBackend() Backend { return &linuxBackend{} }

type linuxBackend struct {
	pulse *pulseAPI
	alsa  *alsaAPI
	mu    sync.Mutex
}

func (b *linuxBackend) Init() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pulse != nil || b.alsa != nil {
		return nil
	}
	p, pe := loadPulse()
	a, ae := loadALSA()
	if pe != nil && ae != nil {
		return errors.Join(pe, ae)
	}
	b.pulse, b.alsa = p, a
	return nil
}

func (b *linuxBackend) ListDevices() ([]DeviceInfo, []DeviceInfo, error) {
	if err := b.Init(); err != nil {
		return nil, nil, err
	}
	if b.pulse != nil && pulseCallbackSupported {
		capture, playback, err := b.pulse.list()
		if err == nil {
			return capture, playback, nil
		}
	}
	if b.alsa != nil {
		return b.alsa.list()
	}
	return nil, nil, errors.New("no PulseAudio server or ALSA library")
}

func (b *linuxBackend) Open(c DeviceConfig, callback DataCallback) (Device, error) {
	if err := b.Init(); err != nil {
		return nil, err
	}
	return openNativeDevice(b, c, callback)
}

func (b *linuxBackend) openStreams(c DeviceConfig) (pcmStream, pcmStream, error) {
	input, output := string(c.InputID), string(c.OutputID)
	if (strings.HasPrefix(input, "pulse:") || input == "") && (strings.HasPrefix(output, "pulse:") || output == "") && b.pulse != nil {
		in, out, err := b.pulse.open(c, strings.TrimPrefix(input, "pulse:"), strings.TrimPrefix(output, "pulse:"))
		if err == nil {
			return in, out, nil
		}
		if input != "" || output != "" {
			return nil, nil, err
		}
	}
	if b.alsa == nil {
		return nil, nil, errors.New("ALSA is not installed and PulseAudio cannot open")
	}
	if strings.HasPrefix(input, "pulse:") || strings.HasPrefix(output, "pulse:") {
		return nil, nil, errors.New("selected PulseAudio device is unavailable")
	}
	if input == "" {
		input = "default"
	} else {
		input = strings.TrimPrefix(input, "alsa:")
	}
	if output == "" {
		output = "default"
	} else {
		output = strings.TrimPrefix(output, "alsa:")
	}
	return b.alsa.open(c, input, output)
}

func (b *linuxBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pulse != nil {
		b.pulse.close()
		b.pulse = nil
	}
	if b.alsa != nil {
		b.alsa.close()
		b.alsa = nil
	}
	return nil
}

func linuxBind(h uintptr, dest any, name string) error {
	sym, err := purego.Dlsym(h, name)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	purego.RegisterFunc(dest, sym)
	return nil
}

type pulseAPI struct {
	core, simple   uintptr
	newMain        func() uintptr
	freeMain       func(uintptr)
	getMainAPI     func(uintptr) uintptr
	iterate        func(uintptr, int32, *int32) int32
	newContext     func(uintptr, string) uintptr
	connect        func(uintptr, uintptr, int32, uintptr) int32
	getState       func(uintptr) int32
	disconnect     func(uintptr)
	unrefContext   func(uintptr)
	getSources     func(uintptr, uintptr, uintptr) uintptr
	getSinks       func(uintptr, uintptr, uintptr) uintptr
	operationState func(uintptr) int32
	unrefOperation func(uintptr)
	newSimple      func(uintptr, string, int32, uintptr, string, *paSampleSpec, uintptr, *paBufferAttr, *int32) uintptr
	freeSimple     func(uintptr)
	readSimple     func(uintptr, []byte, uintptr, *int32) int32
	writeSimple    func(uintptr, []byte, uintptr, *int32) int32
}

type paSampleSpec struct {
	Format   int32
	Rate     uint32
	Channels uint8
	_        [3]byte
}
type paBufferAttr struct{ Maxlength, Tlength, Prebuf, Minreq, Fragsize uint32 }

func loadPulse() (*pulseAPI, error) {
	core, err := purego.Dlopen("libpulse.so.0", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	simple, err := purego.Dlopen("libpulse-simple.so.0", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		_ = purego.Dlclose(core)
		return nil, err
	}
	p := &pulseAPI{core: core, simple: simple}
	for _, entry := range []struct {
		h    uintptr
		f    any
		name string
	}{
		{core, &p.newMain, "pa_mainloop_new"}, {core, &p.freeMain, "pa_mainloop_free"},
		{core, &p.getMainAPI, "pa_mainloop_get_api"}, {core, &p.iterate, "pa_mainloop_iterate"},
		{core, &p.newContext, "pa_context_new"}, {core, &p.connect, "pa_context_connect"},
		{core, &p.getState, "pa_context_get_state"}, {core, &p.disconnect, "pa_context_disconnect"},
		{core, &p.unrefContext, "pa_context_unref"},
		{core, &p.getSources, "pa_context_get_source_info_list"},
		{core, &p.getSinks, "pa_context_get_sink_info_list"},
		{core, &p.operationState, "pa_operation_get_state"}, {core, &p.unrefOperation, "pa_operation_unref"},
		{simple, &p.newSimple, "pa_simple_new"}, {simple, &p.freeSimple, "pa_simple_free"},
		{simple, &p.readSimple, "pa_simple_read"}, {simple, &p.writeSimple, "pa_simple_write"},
	} {
		if err := linuxBind(entry.h, entry.f, entry.name); err != nil {
			p.close()
			return nil, err
		}
	}
	return p, nil
}

func (p *pulseAPI) close() { _ = purego.Dlclose(p.simple); _ = purego.Dlclose(p.core) }

// Introspection runs under a single lock; callback pointers are registered
// once (purego callbacks are process-lifetime allocations).
var pulseInventory struct {
	sync.Mutex
	capture, playback []DeviceInfo
	inputCB, outputCB uintptr
}

func cString(ptr unsafe.Pointer) string {
	if ptr == nil {
		return ""
	}
	n := 0
	for n < 4096 && *(*byte)(unsafe.Add(ptr, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(ptr), n))
}

func pulseInfo(info unsafe.Pointer) DeviceInfo {
	name := cString(*(*unsafe.Pointer)(info))
	desc := cString(*(*unsafe.Pointer)(unsafe.Add(info, 16)))
	if desc == "" {
		desc = name
	}
	return DeviceInfo{ID: DeviceID("pulse:" + name), Name: desc}
}

func (p *pulseAPI) list() ([]DeviceInfo, []DeviceInfo, error) {
	pulseInventory.Lock()
	defer pulseInventory.Unlock()
	if pulseInventory.inputCB == 0 {
		pulseInventory.inputCB = purego.NewCallback(func(_ uintptr, info unsafe.Pointer, eol int32, _ uintptr) {
			if eol == 0 && info != nil {
				pulseInventory.capture = append(pulseInventory.capture, pulseInfo(info))
			}
		})
		pulseInventory.outputCB = purego.NewCallback(func(_ uintptr, info unsafe.Pointer, eol int32, _ uintptr) {
			if eol == 0 && info != nil {
				pulseInventory.playback = append(pulseInventory.playback, pulseInfo(info))
			}
		})
	}
	pulseInventory.capture, pulseInventory.playback = nil, nil
	main := p.newMain()
	if main == 0 {
		return nil, nil, errors.New("pa_mainloop_new failed")
	}
	defer p.freeMain(main)
	ctx := p.newContext(p.getMainAPI(main), "safem\x00")
	if ctx == 0 {
		return nil, nil, errors.New("pa_context_new failed")
	}
	defer p.unrefContext(ctx)
	if p.connect(ctx, 0, 0, 0) < 0 {
		return nil, nil, errors.New("PulseAudio server unavailable")
	}
	defer p.disconnect(ctx)
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		p.iterate(main, 0, nil)
		switch p.getState(ctx) {
		case 4:
			goto ready
		case 5, 6:
			return nil, nil, errors.New("PulseAudio connection failed")
		}
		time.Sleep(4 * time.Millisecond)
	}
	return nil, nil, errors.New("PulseAudio connection timed out")
ready:
	in := p.getSources(ctx, pulseInventory.inputCB, 0)
	out := p.getSinks(ctx, pulseInventory.outputCB, 0)
	if in == 0 || out == 0 {
		if in != 0 {
			p.unrefOperation(in)
		}
		if out != 0 {
			p.unrefOperation(out)
		}
		return nil, nil, errors.New("PulseAudio enumeration failed")
	}
	defer p.unrefOperation(in)
	defer p.unrefOperation(out)
	for time.Now().Before(deadline.Add(700 * time.Millisecond)) {
		p.iterate(main, 0, nil)
		if p.operationState(in) != 0 && p.operationState(out) != 0 {
			return append([]DeviceInfo(nil), pulseInventory.capture...), append([]DeviceInfo(nil), pulseInventory.playback...), nil
		}
		time.Sleep(4 * time.Millisecond)
	}
	return nil, nil, errors.New("PulseAudio enumeration timed out")
}

func (p *pulseAPI) open(c DeviceConfig, input, output string) (pcmStream, pcmStream, error) {
	// PA_SAMPLE_FLOAT32LE=5, PA_STREAM_RECORD=2, PA_STREAM_PLAYBACK=1.
	spec := paSampleSpec{Format: 5, Rate: c.SampleRate, Channels: uint8(c.Channels)}
	periodBytes := c.SampleRate * c.Channels * 4 * c.PeriodMillis / 1000
	buffer := paBufferAttr{Maxlength: ^uint32(0), Tlength: periodBytes * 3, Prebuf: periodBytes, Minreq: periodBytes, Fragsize: periodBytes}
	var code int32
	device := func(s string) []byte {
		if s == "" {
			return nil
		}
		return append([]byte(s), 0)
	}
	inName, outName := device(input), device(output)
	var inPtr, outPtr uintptr
	if len(inName) > 0 {
		inPtr = uintptr(unsafe.Pointer(&inName[0]))
	}
	if len(outName) > 0 {
		outPtr = uintptr(unsafe.Pointer(&outName[0]))
	}
	var in uintptr
	if !c.PlaybackOnly {
		in = p.newSimple(0, "safem\x00", 2, inPtr, "Voice capture\x00", &spec, 0, &buffer, &code)
		runtime.KeepAlive(inName)
		if in == 0 {
			return nil, nil, fmt.Errorf("PulseAudio capture failed (%d)", code)
		}
	}
	out := p.newSimple(0, "safem\x00", 1, outPtr, "Voice playback\x00", &spec, 0, &buffer, &code)
	runtime.KeepAlive(outName)
	if out == 0 {
		if in != 0 {
			p.freeSimple(in)
		}
		return nil, nil, fmt.Errorf("PulseAudio playback failed (%d)", code)
	}
	var capture pcmStream
	if in != 0 {
		capture = &pulseStream{api: p, handle: in}
	}
	return capture, &pulseStream{api: p, handle: out, playback: true}, nil
}

type pulseStream struct {
	api      *pulseAPI
	handle   uintptr
	playback bool
}

func (s *pulseStream) transfer(buf []byte) (int, error) {
	var code int32
	var status int32
	if s.playback {
		status = s.api.writeSimple(s.handle, buf, uintptr(len(buf)), &code)
	} else {
		status = s.api.readSimple(s.handle, buf, uintptr(len(buf)), &code)
	}
	if status < 0 {
		return 0, fmt.Errorf("PulseAudio stream failed (%d)", code)
	}
	return len(buf), nil
}
func (s *pulseStream) close() error { s.api.freeSimple(s.handle); return nil }

type alsaAPI struct {
	h, libc     uintptr
	openPCM     func(*uintptr, string, int32, int32) int32
	closePCM    func(uintptr) int32
	setParams   func(uintptr, int32, int32, uint32, uint32, int32, uint32) int32
	formatValue func(string) int32
	readPCM     func(uintptr, []byte, uintptr) int64
	writePCM    func(uintptr, []byte, uintptr) int64
	recoverPCM  func(uintptr, int32, int32) int32
	deviceHint  func(int32, string, *unsafe.Pointer) int32
	getHint     func(unsafe.Pointer, string) unsafe.Pointer
	freeHint    func(unsafe.Pointer) int32
	freeCString func(unsafe.Pointer)
}

func loadALSA() (*alsaAPI, error) {
	h, err := purego.Dlopen("libasound.so.2", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	a := &alsaAPI{h: h}
	for _, entry := range []struct {
		f    any
		name string
	}{
		{&a.openPCM, "snd_pcm_open"}, {&a.closePCM, "snd_pcm_close"},
		{&a.setParams, "snd_pcm_set_params"}, {&a.formatValue, "snd_pcm_format_value"},
		{&a.readPCM, "snd_pcm_readi"}, {&a.writePCM, "snd_pcm_writei"},
		{&a.recoverPCM, "snd_pcm_recover"}, {&a.deviceHint, "snd_device_name_hint"},
		{&a.getHint, "snd_device_name_get_hint"}, {&a.freeHint, "snd_device_name_free_hint"},
	} {
		if err := linuxBind(h, entry.f, entry.name); err != nil {
			a.close()
			return nil, err
		}
	}
	libc, err := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		a.close()
		return nil, err
	}
	// Keep libc loaded for the lifetime of this backend; on Linux it is also
	// loaded by the process, and this extra reference is released in Close.
	a.libc = libc
	if err := linuxBind(libc, &a.freeCString, "free"); err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

func (a *alsaAPI) close() {
	if a.libc != 0 {
		_ = purego.Dlclose(a.libc)
	}
	_ = purego.Dlclose(a.h)
}

func (a *alsaAPI) list() ([]DeviceInfo, []DeviceInfo, error) {
	var hints unsafe.Pointer
	if status := a.deviceHint(-1, "pcm\x00", &hints); status < 0 {
		return nil, nil, fmt.Errorf("ALSA inventory: %d", status)
	}
	defer a.freeHint(hints)
	var capture, playback []DeviceInfo
	for i := 0; i < 512; i++ {
		hint := *(*unsafe.Pointer)(unsafe.Add(hints, uintptr(i)*unsafe.Sizeof(uintptr(0))))
		if hint == nil {
			break
		}
		read := func(key string) string {
			ptr := a.getHint(hint, key+"\x00")
			if ptr == nil {
				return ""
			}
			value := cString(ptr)
			a.freeCString(ptr)
			return value
		}
		name, desc, direction := read("NAME"), read("DESC"), read("IOID")
		if name == "" || name == "null" {
			continue
		}
		if desc == "" {
			desc = name
		} else {
			desc = strings.SplitN(desc, "\n", 2)[0]
		}
		item := DeviceInfo{ID: DeviceID("alsa:" + name), Name: desc}
		if direction != "Output" {
			capture = append(capture, item)
		}
		if direction != "Input" {
			playback = append(playback, item)
		}
	}
	return capture, playback, nil
}

func (a *alsaAPI) open(c DeviceConfig, input, output string) (pcmStream, pcmStream, error) {
	fmtFloat := a.formatValue("FLOAT_LE\x00")
	if fmtFloat < 0 {
		return nil, nil, errors.New("ALSA FLOAT_LE format unavailable")
	}
	open := func(name string, stream int32) (*alsaStream, error) {
		var pcm uintptr
		// Nonblocking read/write permits Stop to quiesce without a native wait.
		if status := a.openPCM(&pcm, name+"\x00", stream, 1); status < 0 {
			return nil, fmt.Errorf("ALSA open %s: %d", name, status)
		}
		// RW_INTERLEAVED=3. Request a bounded buffering latency; ALSA's
		// plug devices may convert physical formats and sample rates.
		if status := a.setParams(pcm, fmtFloat, 3, c.Channels, c.SampleRate, 1, c.PeriodMillis*3000); status < 0 {
			a.closePCM(pcm)
			return nil, fmt.Errorf("ALSA configure %s: %d", name, status)
		}
		return &alsaStream{api: a, handle: pcm, channels: int(c.Channels), playback: stream == 0}, nil
	}
	var in *alsaStream
	var err error
	if !c.PlaybackOnly {
		in, err = open(input, 1)
		if err != nil {
			return nil, nil, err
		}
	}
	out, err := open(output, 0)
	if err != nil {
		if in != nil {
			_ = in.close()
		}
		return nil, nil, err
	}
	var capture pcmStream
	if in != nil {
		capture = in
	}
	return capture, out, nil
}

type alsaStream struct {
	api      *alsaAPI
	handle   uintptr
	channels int
	playback bool
}

func (s *alsaStream) transfer(buf []byte) (int, error) {
	frames := uintptr(len(buf) / (s.channels * 4))
	if frames == 0 {
		return 0, nil
	}
	var n int64
	if s.playback {
		n = s.api.writePCM(s.handle, buf, frames)
	} else {
		n = s.api.readPCM(s.handle, buf, frames)
	}
	if n < 0 {
		// EAGAIN=-11 is normal on nonblocking streams. ALSA recovers from
		// overrun/underrun and suspension before the next transfer.
		if n == -11 {
			return 0, nil
		}
		if status := s.api.recoverPCM(s.handle, int32(n), 1); status >= 0 {
			return 0, nil
		}
		return 0, fmt.Errorf("ALSA stream failed: %d", n)
	}
	return int(n) * s.channels * 4, nil
}
func (s *alsaStream) close() error { return fmtErrorALSA(s.api.closePCM(s.handle)) }
func fmtErrorALSA(status int32) error {
	if status < 0 {
		return fmt.Errorf("ALSA close: %d", status)
	}
	return nil
}
