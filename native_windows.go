//go:build windows && (amd64 || arm64) && !safem_test_noaudio

package moho

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/ebitengine/purego"
)

func newDefaultBackend() Backend { return &wasapiBackend{} }

type winGUID struct {
	A    uint32
	B, C uint16
	D    [8]byte
}

var (
	clsEnumerator  = winGUID{0xbcde0395, 0xe52f, 0x467c, [8]byte{0x8e, 0x3d, 0xc4, 0x57, 0x92, 0x91, 0x69, 0x2e}}
	iidEnumerator  = winGUID{0xa95664d2, 0x9614, 0x4f35, [8]byte{0xa7, 0x46, 0xde, 0x8d, 0xb6, 0x36, 0x17, 0xe6}}
	iidAudioClient = winGUID{0x1cb9ad4c, 0xdbfa, 0x4c32, [8]byte{0xb1, 0x78, 0xc2, 0xf5, 0x68, 0xa7, 0x03, 0xb2}}
	iidCapture     = winGUID{0xc8adbd64, 0xe71e, 0x48a0, [8]byte{0xa4, 0xde, 0x18, 0x5c, 0x39, 0x5c, 0xd3, 0x17}}
	iidRender      = winGUID{0xf294acfc, 0x3146, 0x4483, [8]byte{0xa7, 0xbf, 0xad, 0xdc, 0xa7, 0xc2, 0x60, 0xe2}}
	keyFriendly    = struct {
		GUID winGUID
		PID  uint32
	}{winGUID{0xa45c254e, 0xdf1c, 0x4efd, [8]byte{0x80, 0x20, 0x67, 0xd1, 0x46, 0xa8, 0x50, 0xe0}}, 14}
)

type wasapiBackend struct {
	coInit       func(uintptr, uint32) int32
	coUninit     func()
	coCreate     func(*winGUID, uintptr, uint32, *winGUID, *uintptr) int32
	coFree       func(uintptr)
	variantClear func(uintptr) int32
	initOnce     sync.Once
	initErr      error
}

func winBind(dll *syscall.LazyDLL, dest any, name string) error {
	p := dll.NewProc(name)
	if err := p.Find(); err != nil {
		return err
	}
	purego.RegisterFunc(dest, p.Addr())
	return nil
}

func (b *wasapiBackend) Init() error {
	b.initOnce.Do(func() {
		dll := syscall.NewLazyDLL("ole32.dll")
		for _, entry := range []struct {
			dst  any
			name string
		}{
			{&b.coInit, "CoInitializeEx"}, {&b.coUninit, "CoUninitialize"},
			{&b.coCreate, "CoCreateInstance"}, {&b.coFree, "CoTaskMemFree"},
			{&b.variantClear, "PropVariantClear"},
		} {
			if err := winBind(dll, entry.dst, entry.name); err != nil {
				b.initErr = err
				break
			}
		}
	})
	return b.initErr
}
func (b *wasapiBackend) Close() error { return nil }

// Native COM and audio buffers stay valid until their respective release calls.
func winPointer(address uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&address))
}

func winCall(obj uintptr, slot uintptr, args ...uintptr) int32 {
	vt := *(*uintptr)(winPointer(obj))
	fn := *(*uintptr)(unsafe.Add(winPointer(vt), slot*unsafe.Sizeof(uintptr(0))))
	var params [10]uintptr
	params[0] = obj
	copy(params[1:], args)
	result, _, _ := purego.SyscallN(fn, params[:len(args)+1]...)
	return int32(result)
}
func winOK(status int32, what string) error {
	if status < 0 {
		return fmt.Errorf("%s: HRESULT 0x%08x", what, uint32(status))
	}
	return nil
}
func winRelease(obj uintptr) {
	if obj != 0 {
		_ = winCall(obj, 2)
	}
}

func (b *wasapiBackend) enterCOM() (func(), error) {
	if err := b.Init(); err != nil {
		return nil, err
	}
	// The calling goroutine must have locked its OS thread first. STA keeps
	// WASAPI activation and GetService on the same COM apartment.
	if err := winOK(b.coInit(0, 2), "CoInitializeEx"); err != nil {
		return nil, err
	}
	return b.coUninit, nil
}

func (b *wasapiBackend) enumerator() (uintptr, error) {
	var obj uintptr
	if err := winOK(b.coCreate(&clsEnumerator, 0, 23, &iidEnumerator, &obj), "MMDeviceEnumerator"); err != nil {
		return 0, err
	}
	return obj, nil
}

func winString(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	length := 0
	for length < 4096 && *(*uint16)(unsafe.Add(winPointer(ptr), uintptr(length)*2)) != 0 {
		length++
	}
	return string(utf16.Decode(unsafe.Slice((*uint16)(winPointer(ptr)), length)))
}

func (b *wasapiBackend) ListDevices() ([]DeviceInfo, []DeviceInfo, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	leave, err := b.enterCOM()
	if err != nil {
		return nil, nil, err
	}
	defer leave()
	enum, err := b.enumerator()
	if err != nil {
		return nil, nil, err
	}
	defer winRelease(enum)
	capture, err := b.enumerate(enum, 1)
	if err != nil {
		return nil, nil, err
	}
	playback, err := b.enumerate(enum, 0)
	return capture, playback, err
}

func (b *wasapiBackend) enumerate(enum uintptr, flow uintptr) ([]DeviceInfo, error) {
	var collection uintptr
	if err := winOK(winCall(enum, 3, flow, 1, uintptr(unsafe.Pointer(&collection))), "EnumAudioEndpoints"); err != nil {
		return nil, err
	}
	defer winRelease(collection)
	var count uint32
	if err := winOK(winCall(collection, 3, uintptr(unsafe.Pointer(&count))), "IEnumMMDevice.GetCount"); err != nil {
		return nil, err
	}
	var items []DeviceInfo
	for i := uint32(0); i < count; i++ {
		var device uintptr
		if winCall(collection, 4, uintptr(i), uintptr(unsafe.Pointer(&device))) < 0 {
			continue
		}
		var idptr uintptr
		if winCall(device, 5, uintptr(unsafe.Pointer(&idptr))) < 0 {
			winRelease(device)
			continue
		}
		id := winString(idptr)
		b.coFree(idptr)
		label := id
		var props uintptr
		if winCall(device, 4, 0, uintptr(unsafe.Pointer(&props))) >= 0 {
			// PROPVARIANT is 24 bytes on amd64 and arm64; VT_LPWSTR=31.
			var prop [24]byte
			if winCall(props, 5, uintptr(unsafe.Pointer(&keyFriendly)), uintptr(unsafe.Pointer(&prop[0]))) >= 0 {
				if *(*uint16)(unsafe.Pointer(&prop[0])) == 31 {
					if ptr := *(*uintptr)(unsafe.Pointer(&prop[8])); ptr != 0 {
						label = winString(ptr)
					}
				}
				b.variantClear(uintptr(unsafe.Pointer(&prop[0])))
			}
			winRelease(props)
		}
		items = append(items, DeviceInfo{ID: DeviceID(id), Name: label})
		winRelease(device)
	}
	return items, nil
}

func (b *wasapiBackend) Open(c DeviceConfig, cb DataCallback) (Device, error) {
	if err := b.Init(); err != nil {
		return nil, err
	}
	if cb == nil || c.SampleRate == 0 || c.Channels == 0 || c.PeriodMillis == 0 {
		return nil, errors.New("invalid WASAPI configuration")
	}
	return &wasapiDevice{api: b, config: c, callback: cb}, nil
}

type wasapiDevice struct {
	api      *wasapiBackend
	config   DeviceConfig
	callback DataCallback
	mu       sync.Mutex
	stop     chan struct{}
	done     chan struct{}
	closed   bool
}

func (d *wasapiDevice) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("WASAPI device closed")
	}
	if d.done != nil {
		return errors.New("WASAPI device already running")
	}
	d.stop, d.done = make(chan struct{}), make(chan struct{})
	ready := make(chan error, 1)
	go d.run(d.stop, d.done, ready)
	if err := <-ready; err != nil {
		<-d.done
		d.done = nil
		return err
	}
	return nil
}
func (d *wasapiDevice) Stop() error {
	d.mu.Lock()
	done := d.done
	if done != nil {
		close(d.stop)
		d.done = nil
	}
	d.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}
func (d *wasapiDevice) Close() error {
	_ = d.Stop()
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

type waveFormat struct {
	Tag, Channels      uint16
	Rate, BytesPerSec  uint32
	Align, Bits, Extra uint16
}
type wasapiStream struct {
	client, service uintptr
	frames          uint32
}

func (s *wasapiStream) close() {
	if s.client != 0 {
		_ = winCall(s.client, 11)
	}
	winRelease(s.service)
	winRelease(s.client)
}

func (d *wasapiDevice) openStream(enum uintptr, id DeviceID, flow uintptr) (result wasapiStream, err error) {
	var dev uintptr
	if id == "" {
		err = winOK(winCall(enum, 4, flow, 0, uintptr(unsafe.Pointer(&dev))), "GetDefaultAudioEndpoint")
	} else {
		str, convErr := syscall.UTF16PtrFromString(string(id))
		if convErr != nil {
			return result, convErr
		}
		err = winOK(winCall(enum, 5, uintptr(unsafe.Pointer(str)), uintptr(unsafe.Pointer(&dev))), "GetDevice")
	}
	if err != nil {
		return result, err
	}
	defer winRelease(dev)
	if err = winOK(winCall(dev, 3, uintptr(unsafe.Pointer(&iidAudioClient)), 23, 0, uintptr(unsafe.Pointer(&result.client))), "IMMDevice.Activate"); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			result.close()
		}
	}()
	format := waveFormat{Tag: 3, Channels: uint16(d.config.Channels), Rate: d.config.SampleRate, Bits: 32, Align: uint16(d.config.Channels * 4), BytesPerSec: d.config.SampleRate * d.config.Channels * 4}
	// Shared-mode engine converts channel layout and rate to the device mix.
	const flags = 0x80000000 | 0x08000000
	if err = winOK(winCall(result.client, 3, 0, flags, 1000000, 0, uintptr(unsafe.Pointer(&format)), 0), "IAudioClient.Initialize"); err != nil {
		return result, err
	}
	if err = winOK(winCall(result.client, 4, uintptr(unsafe.Pointer(&result.frames))), "IAudioClient.GetBufferSize"); err != nil {
		return result, err
	}
	iid := &iidCapture
	if flow == 0 {
		iid = &iidRender
	}
	if err = winOK(winCall(result.client, 14, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&result.service))), "IAudioClient.GetService"); err != nil {
		return result, err
	}
	return result, nil
}

func (d *wasapiDevice) defaultEndpointID(enum, flow uintptr) (string, error) {
	var device uintptr
	if err := winOK(winCall(enum, 4, flow, 0, uintptr(unsafe.Pointer(&device))), "GetDefaultAudioEndpoint"); err != nil {
		return "", err
	}
	defer winRelease(device)
	var ptr uintptr
	if err := winOK(winCall(device, 5, uintptr(unsafe.Pointer(&ptr))), "IMMDevice.GetId"); err != nil {
		return "", err
	}
	defer d.api.coFree(ptr)
	return winString(ptr), nil
}

func (d *wasapiDevice) run(stop <-chan struct{}, done chan<- struct{}, ready chan<- error) {
	defer close(done)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	leave, err := d.api.enterCOM()
	if err != nil {
		ready <- err
		return
	}
	defer leave()
	first := true
	for {
		select {
		case <-stop:
			return
		default:
		}
		initial := first
		err = d.session(stop, func(err error) {
			if first {
				ready <- err
				first = false
			}
		})
		if first {
			ready <- err
			return
		}
		if initial && err != nil {
			return
		}
		if err == nil {
			return
		}
		// Device invalidation (including default-device changes) reopens both
		// endpoint streams after a bounded delay.
		select {
		case <-stop:
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (d *wasapiDevice) session(stop <-chan struct{}, signal func(error)) error {
	enum, err := d.api.enumerator()
	if err != nil {
		signal(err)
		return err
	}
	defer winRelease(enum)
	var in wasapiStream
	if !d.config.PlaybackOnly {
		in, err = d.openStream(enum, d.config.InputID, 1)
		if err != nil {
			signal(err)
			return err
		}
		defer in.close()
	}
	out, err := d.openStream(enum, d.config.OutputID, 0)
	if err != nil {
		signal(err)
		return err
	}
	defer out.close()
	if !d.config.PlaybackOnly {
		if err = winOK(winCall(in.client, 10), "capture Start"); err != nil {
			signal(err)
			return err
		}
	}
	if err = winOK(winCall(out.client, 10), "render Start"); err != nil {
		signal(err)
		return err
	}
	var defaultIn, defaultOut string
	if !d.config.PlaybackOnly && d.config.InputID == "" {
		defaultIn, err = d.defaultEndpointID(enum, 1)
		if err != nil {
			signal(err)
			return err
		}
	}
	if d.config.OutputID == "" {
		defaultOut, err = d.defaultEndpointID(enum, 0)
		if err != nil {
			signal(err)
			return err
		}
	}
	signal(nil)
	frames := int(d.config.SampleRate * d.config.PeriodMillis / 1000)
	if frames < 1 {
		frames = 1
	}
	var q *captureQueue
	var silence []byte
	if !d.config.PlaybackOnly {
		q = &captureQueue{data: make([]byte, frames*int(d.config.Channels)*4*5)}
		silence = make([]byte, len(q.data))
	}
	input, output := make([]byte, frames*int(d.config.Channels)*4), make([]byte, frames*int(d.config.Channels)*4)
	nextCheck := time.Now().Add(time.Second)
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		if q != nil {
			if err = d.capture(in, q, silence); err != nil {
				return err
			}
		}
		if err = d.render(out, q, input, output, frames); err != nil {
			return err
		}
		if time.Now().After(nextCheck) {
			nextCheck = time.Now().Add(time.Second)
			if defaultIn != "" {
				id, e := d.defaultEndpointID(enum, 1)
				if e != nil || id != defaultIn {
					return errors.New("default capture endpoint changed")
				}
			}
			if defaultOut != "" {
				id, e := d.defaultEndpointID(enum, 0)
				if e != nil || id != defaultOut {
					return errors.New("default render endpoint changed")
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (d *wasapiDevice) capture(s wasapiStream, q *captureQueue, silence []byte) error {
	for {
		var count uint32
		if err := winOK(winCall(s.service, 5, uintptr(unsafe.Pointer(&count))), "capture packet size"); err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		var data uintptr
		var frames, flags uint32
		if err := winOK(winCall(s.service, 3, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&frames)), uintptr(unsafe.Pointer(&flags)), 0, 0), "capture GetBuffer"); err != nil {
			return err
		}
		bytes := int(frames * d.config.Channels * 4)
		if flags&2 != 0 || data == 0 {
			for n := bytes; n > 0; {
				chunk := min(n, len(silence))
				q.push(silence[:chunk])
				n -= chunk
			}
		} else {
			q.push(unsafe.Slice((*byte)(winPointer(data)), bytes))
		}
		if err := winOK(winCall(s.service, 4, uintptr(frames)), "capture ReleaseBuffer"); err != nil {
			return err
		}
	}
}

func (d *wasapiDevice) render(s wasapiStream, q *captureQueue, input, output []byte, period int) error {
	var padding uint32
	if err := winOK(winCall(s.client, 6, uintptr(unsafe.Pointer(&padding))), "render padding"); err != nil {
		return err
	}
	if padding > s.frames {
		return errors.New("invalid WASAPI render padding")
	}
	available := int(s.frames - padding)
	if available < period {
		return nil
	}
	nframes := min(period, available)
	size := nframes * int(d.config.Channels) * 4
	if q != nil {
		q.pop(input[:size])
		input = input[:size]
	} else {
		input = nil
	}
	clear(output[:size])
	d.callback(output[:size], input, uint32(nframes))
	var data uintptr
	if err := winOK(winCall(s.service, 3, uintptr(nframes), uintptr(unsafe.Pointer(&data))), "render GetBuffer"); err != nil {
		return err
	}
	copy(unsafe.Slice((*byte)(winPointer(data)), size), output[:size])
	return winOK(winCall(s.service, 4, uintptr(nframes), 0), "render ReleaseBuffer")
}
