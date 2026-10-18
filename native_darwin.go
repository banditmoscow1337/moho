//go:build darwin && (amd64 || arm64) && !safem_test_noaudio

package audio

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

func newDefaultBackend() Backend { return &coreAudioBackend{} }

const (
	macGlobal        = uint32(0x676c6f62) // 'glob'
	macInput         = uint32(0x696e7074) // 'inpt'
	macOutput        = uint32(0x6f757470) // 'outp'
	macDevices       = uint32(0x64657623) // 'dev#'
	macStreams       = uint32(0x73746d23) // 'stm#'
	macName          = uint32(0x6c6e616d) // 'lnam'
	macDefaultInput  = uint32(0x64496e20) // 'dIn '
	macDefaultOutput = uint32(0x644f7574) // 'dOut'
)

type macAddress struct{ Selector, Scope, Element uint32 }
type macDescription struct{ Type, SubType, Manufacturer, Flags, FlagsMask uint32 }
type macFormat struct {
	Rate                                                                                float64
	ID, Flags, BytesPerPacket, FramesPerPacket, BytesPerFrame, Channels, Bits, Reserved uint32
}
type macCallback struct{ Proc, Refcon uintptr }
type macBuffer struct {
	Channels, Size uint32
	Data           unsafe.Pointer
}
type macBufferList struct {
	Count  uint32
	_      uint32
	Buffer macBuffer
}

type coreAudioBackend struct {
	mu                        sync.Mutex
	core, toolbox, foundation uintptr
	playbackUnderruns         atomic.Uint64
	playbackMissingFrames     atomic.Uint64
	propertySize              func(uint32, *macAddress, uint32, uintptr, *uint32) int32
	property                  func(uint32, *macAddress, uint32, uintptr, *uint32, uintptr) int32
	stringCopy                func(uintptr, []byte, int64, uint32) int32
	releaseCF                 func(uintptr)
	findComponent             func(uintptr, *macDescription) uintptr
	newInstance               func(uintptr, *uintptr) int32
	dispose                   func(uintptr) int32
	setProperty               func(uintptr, uint32, uint32, uint32, uintptr, uint32) int32
	initialize                func(uintptr) int32
	uninitialize              func(uintptr) int32
	start                     func(uintptr) int32
	stop                      func(uintptr) int32
	render                    func(uintptr, uintptr, uintptr, uint32, uint32, uintptr) int32
}

func (b *coreAudioBackend) outputUnderruns() (uint64, uint64) {
	return b.playbackUnderruns.Load(), b.playbackMissingFrames.Load()
}

func macBind(h uintptr, dest any, name string) error {
	sym, err := purego.Dlsym(h, name)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	purego.RegisterFunc(dest, sym)
	return nil
}

func (b *coreAudioBackend) Init() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.core != 0 {
		return nil
	}
	var err error
	b.core, err = purego.Dlopen("/System/Library/Frameworks/CoreAudio.framework/CoreAudio", purego.RTLD_NOW)
	if err != nil {
		return err
	}
	b.toolbox, err = purego.Dlopen("/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox", purego.RTLD_NOW)
	if err != nil {
		b.unload()
		return err
	}
	b.foundation, err = purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW)
	if err != nil {
		b.unload()
		return err
	}
	for _, s := range []struct {
		h    uintptr
		f    any
		name string
	}{
		{b.core, &b.propertySize, "AudioObjectGetPropertyDataSize"},
		{b.core, &b.property, "AudioObjectGetPropertyData"},
		{b.foundation, &b.stringCopy, "CFStringGetCString"},
		{b.foundation, &b.releaseCF, "CFRelease"},
		{b.toolbox, &b.findComponent, "AudioComponentFindNext"},
		{b.toolbox, &b.newInstance, "AudioComponentInstanceNew"},
		{b.toolbox, &b.dispose, "AudioComponentInstanceDispose"},
		{b.toolbox, &b.setProperty, "AudioUnitSetProperty"},
		{b.toolbox, &b.initialize, "AudioUnitInitialize"},
		{b.toolbox, &b.uninitialize, "AudioUnitUninitialize"},
		{b.toolbox, &b.start, "AudioOutputUnitStart"},
		{b.toolbox, &b.stop, "AudioOutputUnitStop"},
		{b.toolbox, &b.render, "AudioUnitRender"},
	} {
		if err := macBind(s.h, s.f, s.name); err != nil {
			b.unload()
			return err
		}
	}
	return nil
}

func (b *coreAudioBackend) unload() {
	if b.foundation != 0 {
		_ = purego.Dlclose(b.foundation)
		b.foundation = 0
	}
	if b.toolbox != 0 {
		_ = purego.Dlclose(b.toolbox)
		b.toolbox = 0
	}
	if b.core != 0 {
		_ = purego.Dlclose(b.core)
		b.core = 0
	}
}
func (b *coreAudioBackend) Close() error { b.mu.Lock(); defer b.mu.Unlock(); b.unload(); return nil }

func (b *coreAudioBackend) deviceProperty(id, selector, scope uint32, out uintptr) error {
	addr := macAddress{Selector: selector, Scope: scope}
	size := uint32(4)
	if selector == macName {
		size = uint32(unsafe.Sizeof(uintptr(0)))
	}
	if status := b.property(id, &addr, 0, 0, &size, out); status != 0 {
		return fmt.Errorf("CoreAudio property %x: %d", selector, status)
	}
	return nil
}

func (b *coreAudioBackend) deviceID(id DeviceID, selector uint32) (uint32, error) {
	if id != "" {
		n, err := strconv.ParseUint(string(id), 10, 32)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("invalid CoreAudio device %q", id)
		}
		return uint32(n), nil
	}
	var result uint32
	if err := b.deviceProperty(1, selector, macGlobal, uintptr(unsafe.Pointer(&result))); err != nil {
		return 0, err
	}
	return result, nil
}

func (b *coreAudioBackend) ListDevices() ([]DeviceInfo, []DeviceInfo, error) {
	if err := b.Init(); err != nil {
		return nil, nil, err
	}
	addr := macAddress{Selector: macDevices, Scope: macGlobal}
	var size uint32
	if status := b.propertySize(1, &addr, 0, 0, &size); status != 0 {
		return nil, nil, fmt.Errorf("CoreAudio device list: %d", status)
	}
	if size == 0 {
		return nil, nil, nil
	}
	ids := make([]uint32, size/4)
	if status := b.property(1, &addr, 0, 0, &size, uintptr(unsafe.Pointer(&ids[0]))); status != 0 {
		return nil, nil, fmt.Errorf("CoreAudio device IDs: %d", status)
	}
	var capture, playback []DeviceInfo
	for _, id := range ids[:size/4] {
		name := strconv.FormatUint(uint64(id), 10)
		label := name
		var cf uintptr
		if b.deviceProperty(id, macName, macGlobal, uintptr(unsafe.Pointer(&cf))) == nil && cf != 0 {
			buf := make([]byte, 512)
			if b.stringCopy(cf, buf, int64(len(buf)), 0x08000100) != 0 {
				for n := 0; n < len(buf); n++ {
					if buf[n] == 0 {
						label = string(buf[:n])
						break
					}
				}
			}
			b.releaseCF(cf)
		}
		for _, direction := range []struct {
			scope uint32
			dst   *[]DeviceInfo
		}{
			{macInput, &capture}, {macOutput, &playback},
		} {
			streams := macAddress{Selector: macStreams, Scope: direction.scope}
			var bytes uint32
			if b.propertySize(id, &streams, 0, 0, &bytes) == 0 && bytes > 0 {
				*direction.dst = append(*direction.dst, DeviceInfo{ID: DeviceID(name), Name: label})
			}
		}
	}
	return capture, playback, nil
}

func (b *coreAudioBackend) Open(c DeviceConfig, cb DataCallback) (Device, error) {
	if err := b.Init(); err != nil {
		return nil, err
	}
	return openNativeDevice(b, c, cb)
}

func (b *coreAudioBackend) openStreams(c DeviceConfig) (pcmStream, pcmStream, error) {
	var in pcmStream
	if !c.PlaybackOnly {
		inID, err := b.deviceID(c.InputID, macDefaultInput)
		if err != nil {
			return nil, nil, err
		}
		in, err = b.openUnit(c, inID, false)
		if err != nil {
			return nil, nil, err
		}
	}
	outID, err := b.deviceID(c.OutputID, macDefaultOutput)
	if err != nil {
		if in != nil {
			_ = in.close()
		}
		return nil, nil, err
	}
	out, err := b.openUnit(c, outID, true)
	if err != nil {
		if in != nil {
			_ = in.close()
		}
		return nil, nil, err
	}
	return in, out, nil
}

var macCallbacks struct {
	sync.Map
	next          atomic.Uint64
	input, output uintptr
	once          sync.Once
}

func macCallbacksInit() {
	macCallbacks.once.Do(func() {
		macCallbacks.input = purego.NewCallback(func(refcon, flags, stamp uintptr, _ uint32, frames uint32, _ uintptr) int32 {
			if v, ok := macCallbacks.Load(refcon); ok {
				v.(*macStream).receive(flags, stamp, frames)
			}
			return 0
		})
		macCallbacks.output = purego.NewCallback(func(refcon, _ uintptr, _ uintptr, _ uint32, frames uint32, data unsafe.Pointer) int32 {
			if v, ok := macCallbacks.Load(refcon); ok {
				v.(*macStream).renderOutput(frames, data)
			}
			return 0
		})
	})
}

type macStream struct {
	api          *coreAudioBackend
	unit         uintptr
	id           uintptr
	playback     bool
	fifo         macFIFO
	inputBuffer  []byte
	channels     int
	lastCallback atomic.Int64
	failed       atomic.Bool
	deviceID     uint32
	defaultScope uint32
	checkStop    chan struct{}
	checkDone    chan struct{}
}

func (b *coreAudioBackend) openUnit(c DeviceConfig, id uint32, playback bool) (_ *macStream, err error) {
	macCallbacksInit()
	component := b.findComponent(0, &macDescription{Type: 0x61756f75, SubType: 0x6168616c, Manufacturer: 0x6170706c}) // 'auou','ahal','appl'
	if component == 0 {
		return nil, errors.New("AUHAL component unavailable")
	}
	var unit uintptr
	if status := b.newInstance(component, &unit); status != 0 {
		return nil, fmt.Errorf("AUHAL instance: %d", status)
	}
	queueBytes := int(c.SampleRate*c.Channels*4*c.PeriodMillis/1000) * 4
	s := &macStream{api: b, unit: unit, playback: playback, fifo: macFIFO{data: make([]byte, queueBytes)}, inputBuffer: make([]byte, 8192*int(c.Channels)*4), channels: int(c.Channels)}
	s.deviceID = id
	if playback && c.OutputID == "" {
		s.defaultScope = macDefaultOutput
	}
	if !playback && c.InputID == "" {
		s.defaultScope = macDefaultInput
	}
	s.lastCallback.Store(time.Now().UnixNano())
	defer func() {
		if err != nil {
			_ = s.close()
		}
	}()
	set := func(property, scope, element uint32, value uintptr, size uint32) error {
		if status := b.setProperty(unit, property, scope, element, value, size); status != 0 {
			return fmt.Errorf("AUHAL property %d: %d", property, status)
		}
		return nil
	}
	var enable uint32 = 1
	if !playback {
		if err = set(2003, 1, 1, uintptr(unsafe.Pointer(&enable)), 4); err != nil {
			return nil, err
		}
		enable = 0
		if err = set(2003, 2, 0, uintptr(unsafe.Pointer(&enable)), 4); err != nil {
			return nil, err
		}
	}
	if err = set(2000, 0, 0, uintptr(unsafe.Pointer(&id)), 4); err != nil {
		return nil, err
	}
	format := macFormat{Rate: float64(c.SampleRate), ID: 0x6c70636d, Flags: 9, BytesPerPacket: c.Channels * 4, FramesPerPacket: 1, BytesPerFrame: c.Channels * 4, Channels: c.Channels, Bits: 32}
	if playback {
		err = set(8, 1, 0, uintptr(unsafe.Pointer(&format)), uint32(unsafe.Sizeof(format)))
	} else {
		err = set(8, 2, 1, uintptr(unsafe.Pointer(&format)), uint32(unsafe.Sizeof(format)))
	}
	if err != nil {
		return nil, err
	}
	s.id = uintptr(macCallbacks.next.Add(1))
	macCallbacks.Store(s.id, s)
	proc := macCallbacks.input
	if playback {
		proc = macCallbacks.output
	}
	cb := macCallback{Proc: proc, Refcon: s.id}
	if playback {
		err = set(23, 1, 0, uintptr(unsafe.Pointer(&cb)), uint32(unsafe.Sizeof(cb)))
	} else {
		err = set(2005, 0, 0, uintptr(unsafe.Pointer(&cb)), uint32(unsafe.Sizeof(cb)))
	}
	if err != nil {
		return nil, err
	}
	if status := b.initialize(unit); status != 0 {
		return nil, fmt.Errorf("AUHAL initialize: %d", status)
	}
	if status := b.start(unit); status != 0 {
		return nil, fmt.Errorf("AUHAL start: %d", status)
	}
	s.startDefaultMonitor()
	return s, nil
}

// macFIFO has one callback-side and one worker-side user per unit.
type macFIFO struct {
	mu   sync.Mutex
	data []byte
	used int
}

func (q *macFIFO) write(buf []byte) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := copy(q.data[q.used:], buf)
	q.used += n
	return n
}
func (q *macFIFO) read(buf []byte) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := copy(buf, q.data[:q.used])
	copy(q.data, q.data[n:q.used])
	q.used -= n
	return n
}
func (q *macFIFO) pushLatest(buf []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(buf) > len(q.data) {
		buf = buf[len(buf)-len(q.data):]
	}
	if excess := q.used + len(buf) - len(q.data); excess > 0 {
		copy(q.data, q.data[excess:q.used])
		q.used -= excess
	}
	q.used += copy(q.data[q.used:], buf)
}
func (s *macStream) transfer(buf []byte) (int, error) {
	if s.failed.Load() {
		return 0, errors.New("CoreAudio render failed")
	}
	if time.Since(time.Unix(0, s.lastCallback.Load())) > 2*time.Second {
		return 0, errors.New("CoreAudio device stopped calling back")
	}
	if s.playback {
		return s.fifo.write(buf), nil
	}
	return s.fifo.read(buf), nil
}

// Querying CoreAudio's default device can block. Keep it off the goroutine
// that fills the playback FIFO; a failed check makes the transfer loop reopen.
func (s *macStream) startDefaultMonitor() {
	if s.defaultScope == 0 {
		return
	}
	s.checkStop = make(chan struct{})
	s.checkDone = make(chan struct{})
	go func() {
		defer close(s.checkDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.checkStop:
				return
			default:
			}
			id, err := s.api.deviceID("", s.defaultScope)
			if err != nil || id != s.deviceID {
				s.failed.Store(true)
				return
			}
			select {
			case <-s.checkStop:
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *macStream) stopDefaultMonitor() {
	if s.checkStop != nil {
		close(s.checkStop)
		<-s.checkDone
		s.checkStop = nil
	}
}
func (s *macStream) receive(flags, stamp uintptr, frames uint32) {
	s.lastCallback.Store(time.Now().UnixNano())
	size := int(frames) * s.channels * 4
	if size > len(s.inputBuffer) {
		s.failed.Store(true)
		return
	}
	data := macBufferList{Count: 1, Buffer: macBuffer{Channels: uint32(s.channels), Size: uint32(size), Data: unsafe.Pointer(&s.inputBuffer[0])}}
	if s.api.render(s.unit, flags, stamp, 1, frames, uintptr(unsafe.Pointer(&data))) == 0 {
		s.fifo.pushLatest(s.inputBuffer[:size])
	} else {
		s.failed.Store(true)
	}
}
func (s *macStream) renderOutput(frames uint32, ptr unsafe.Pointer) {
	s.lastCallback.Store(time.Now().UnixNano())
	if ptr == nil {
		return
	}
	list := (*macBufferList)(ptr)
	if list.Count == 0 || list.Buffer.Data == nil {
		return
	}
	size := min(int(frames)*s.channels*4, int(list.Buffer.Size))
	buf := unsafe.Slice((*byte)(list.Buffer.Data), size)
	n := s.fifo.read(buf)
	clear(buf[n:])
	if n < size {
		s.api.playbackUnderruns.Add(1)
		s.api.playbackMissingFrames.Add(uint64((size - n) / (s.channels * 4)))
	}
}
func (s *macStream) close() error {
	if s.unit == 0 {
		return nil
	}
	s.stopDefaultMonitor()
	_ = s.api.stop(s.unit)
	macCallbacks.Delete(s.id)
	_ = s.api.uninitialize(s.unit)
	status := s.api.dispose(s.unit)
	s.unit = 0
	if status != 0 {
		return fmt.Errorf("AUHAL dispose: %d", status)
	}
	return nil
}
