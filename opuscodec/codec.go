//go:build (darwin || linux || windows) && (amd64 || arm64)

// Package opuscodec binds libopus with purego. Each encoder and decoder owns a
// native Opus state; callers must Close it after the last call.
package opuscodec

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	applicationVoIP int32 = 2048
	opusAllocFail   int32 = -7
)

var (
	ErrClosed              = errors.New("opus codec is closed")
	errInvalidEncodeBuffer = errors.New("opus_encode_float: invalid PCM frame or packet buffer")
	errInvalidDecodeBuffer = errors.New("opus_decode_float: invalid PCM frame or packet buffer")
)

type functions struct {
	encoderCreate  uintptr
	encoderDestroy uintptr
	encodeFloat    uintptr
	decoderCreate  uintptr
	decoderDestroy uintptr
	decodeFloat    uintptr
	strerror       func(int32) string
	version        func() string
}

var shared struct {
	sync.Mutex
	f atomic.Pointer[functions]
}

// noescape hides a pointer from escape analysis so the fixed argument array
// passed to purego.SyscallN remains on the stack.
//
//go:nosplit
func noescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

func syscall1(fn, a1 uintptr) uintptr {
	var args [1]uintptr
	args[0] = a1
	s := unsafe.Slice((*uintptr)(noescape(unsafe.Pointer(&args[0]))), 1)
	r1, _, _ := purego.SyscallN(fn, s...)
	return r1
}

func syscall3(fn, a1, a2, a3 uintptr) uintptr {
	var args [3]uintptr
	args[0] = a1
	args[1] = a2
	args[2] = a3
	s := unsafe.Slice((*uintptr)(noescape(unsafe.Pointer(&args[0]))), 3)
	r1, _, _ := purego.SyscallN(fn, s...)
	return r1
}

func syscall4(fn, a1, a2, a3, a4 uintptr) uintptr {
	var args [4]uintptr
	args[0] = a1
	args[1] = a2
	args[2] = a3
	args[3] = a4
	s := unsafe.Slice((*uintptr)(noescape(unsafe.Pointer(&args[0]))), 4)
	r1, _, _ := purego.SyscallN(fn, s...)
	return r1
}

func syscall5(fn, a1, a2, a3, a4, a5 uintptr) uintptr {
	var args [5]uintptr
	args[0] = a1
	args[1] = a2
	args[2] = a3
	args[3] = a4
	args[4] = a5
	s := unsafe.Slice((*uintptr)(noescape(unsafe.Pointer(&args[0]))), 5)
	r1, _, _ := purego.SyscallN(fn, s...)
	return r1
}

func syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) uintptr {
	var args [6]uintptr
	args[0] = a1
	args[1] = a2
	args[2] = a3
	args[3] = a4
	args[4] = a5
	args[5] = a6
	s := unsafe.Slice((*uintptr)(noescape(unsafe.Pointer(&args[0]))), 6)
	r1, _, _ := purego.SyscallN(fn, s...)
	return r1
}

// getFunctions retries after a missing library is installed. A successfully
// loaded library stays resident so registered function pointers remain valid.
func getFunctions() (*functions, error) {
	if f := shared.f.Load(); f != nil {
		return f, nil
	}
	shared.Lock()
	defer shared.Unlock()
	if f := shared.f.Load(); f != nil {
		return f, nil
	}
	lib, err := openLibrary()
	if err != nil {
		return nil, err
	}
	var f functions
	for _, symbol := range []struct {
		name string
		dst  any
	}{
		{"opus_encoder_create", &f.encoderCreate},
		{"opus_encoder_destroy", &f.encoderDestroy},
		{"opus_encode_float", &f.encodeFloat},
		{"opus_decoder_create", &f.decoderCreate},
		{"opus_decoder_destroy", &f.decoderDestroy},
		{"opus_decode_float", &f.decodeFloat},
		{"opus_strerror", &f.strerror},
		{"opus_get_version_string", &f.version},
	} {
		if err := lib.bind(symbol.dst, symbol.name); err != nil {
			_ = lib.close()
			return nil, fmt.Errorf("libopus: %w", err)
		}
	}
	shared.f.Store(&f)
	return &f, nil
}

// Version reports the loaded libopus version and also verifies that its
// required symbols can be resolved. This does not open an audio device.
func Version() (string, error) {
	f, err := getFunctions()
	if err != nil {
		return "", err
	}
	return f.version(), nil
}

func validFormat(rate, channels int) bool {
	switch rate {
	case 8000, 12000, 16000, 24000, 48000:
		return channels == 1 || channels == 2
	}
	return false
}

func opusError(f *functions, operation string, code int32) error {
	return fmt.Errorf("%s: %s (code %d)", operation, f.strerror(code), code)
}

type Encoder struct {
	state    uintptr
	channels int
}

func NewEncoder(rate, channels int) (*Encoder, error) {
	state, err := newEncoderState(rate, channels)
	if err != nil {
		return nil, err
	}
	return &Encoder{state, channels}, nil
}

func newEncoderState(rate, channels int) (uintptr, error) {
	if !validFormat(rate, channels) {
		return 0, fmt.Errorf("opus encoder: invalid rate %d or channel count %d", rate, channels)
	}
	f, err := getFunctions()
	if err != nil {
		return 0, err
	}
	state := syscall4(f.encoderCreate, uintptr(rate), uintptr(channels), uintptr(applicationVoIP), 0)
	if state == 0 {
		return 0, opusError(f, "opus_encoder_create", opusAllocFail)
	}
	return state, nil
}

func (e *Encoder) EncodeFloat32(pcm []float32, packet []byte) (int, error) {
	if e == nil || e.state == 0 {
		return 0, ErrClosed
	}
	if len(pcm) == 0 || len(pcm)%e.channels != 0 || len(pcm)/e.channels > math.MaxInt32 || len(packet) == 0 || len(packet) > math.MaxInt32 {
		return 0, errInvalidEncodeBuffer
	}
	f := shared.f.Load()
	r1 := syscall5(
		f.encodeFloat,
		e.state,
		uintptr(unsafe.Pointer(&pcm[0])),
		uintptr(len(pcm)/e.channels),
		uintptr(unsafe.Pointer(&packet[0])),
		uintptr(len(packet)),
	)
	runtime.KeepAlive(pcm)
	runtime.KeepAlive(packet)
	n := int32(r1)
	if n < 0 {
		return 0, opusError(f, "opus_encode_float", n)
	}
	return int(n), nil
}

func (e *Encoder) Close() error {
	if e != nil && e.state != 0 {
		e.close()
	}
	return nil
}

func (e *Encoder) close() {
	s := e.state
	e.state = 0
	syscall1(shared.f.Load().encoderDestroy, s)
}

type Decoder struct {
	state    uintptr
	channels int
}

func NewDecoder(rate, channels int) (*Decoder, error) {
	state, err := newDecoderState(rate, channels)
	if err != nil {
		return nil, err
	}
	return &Decoder{state, channels}, nil
}

func newDecoderState(rate, channels int) (uintptr, error) {
	if !validFormat(rate, channels) {
		return 0, fmt.Errorf("opus decoder: invalid rate %d or channel count %d", rate, channels)
	}
	f, err := getFunctions()
	if err != nil {
		return 0, err
	}
	state := syscall3(f.decoderCreate, uintptr(rate), uintptr(channels), 0)
	if state == 0 {
		return 0, opusError(f, "opus_decoder_create", opusAllocFail)
	}
	return state, nil
}

func (d *Decoder) DecodeFloat32(packet []byte, pcm []float32) (int, error) {
	if d == nil || d.state == 0 {
		return 0, ErrClosed
	}
	if len(pcm) == 0 || len(pcm)%d.channels != 0 || len(pcm)/d.channels > math.MaxInt32 || len(packet) > math.MaxInt32 {
		return 0, errInvalidDecodeBuffer
	}
	var data uintptr
	// A nil packet requests packet loss concealment.
	if len(packet) != 0 {
		data = uintptr(unsafe.Pointer(&packet[0]))
	}
	f := shared.f.Load()
	r1 := syscall6(
		f.decodeFloat,
		d.state,
		data,
		uintptr(len(packet)),
		uintptr(unsafe.Pointer(&pcm[0])),
		uintptr(len(pcm)/d.channels),
		0,
	)
	runtime.KeepAlive(packet)
	runtime.KeepAlive(pcm)
	n := int32(r1)
	if n < 0 {
		return 0, opusError(f, "opus_decode_float", n)
	}
	return int(n), nil
}

func (d *Decoder) Close() error {
	if d != nil && d.state != 0 {
		d.close()
	}
	return nil
}

func (d *Decoder) close() {
	s := d.state
	d.state = 0
	syscall1(shared.f.Load().decoderDestroy, s)
}