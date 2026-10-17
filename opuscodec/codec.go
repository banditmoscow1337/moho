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
)

const applicationVoIP int32 = 2048

var ErrClosed = errors.New("opus codec is closed")

type functions struct {
	encoderCreate  func(int32, int32, int32, *int32) uintptr
	encoderDestroy func(uintptr)
	encodeFloat    func(uintptr, *float32, int32, *byte, int32) int32
	decoderCreate  func(int32, int32, *int32) uintptr
	decoderDestroy func(uintptr)
	decodeFloat    func(uintptr, *byte, int32, *float32, int32, int32) int32
	strerror       func(int32) string
	version        func() string
}

var shared struct {
	sync.Mutex
	f *functions
}

// getFunctions retries after a missing library is installed. A successfully
// loaded library stays resident so registered function pointers remain valid.
func getFunctions() (*functions, error) {
	shared.Lock()
	defer shared.Unlock()
	if shared.f != nil {
		return shared.f, nil
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
	shared.f = &f
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
	f        *functions
	state    uintptr
	channels int
}

func NewEncoder(rate, channels int) (*Encoder, error) {
	if !validFormat(rate, channels) {
		return nil, fmt.Errorf("opus encoder: invalid rate %d or channel count %d", rate, channels)
	}
	f, err := getFunctions()
	if err != nil {
		return nil, err
	}
	var code int32
	state := f.encoderCreate(int32(rate), int32(channels), applicationVoIP, &code)
	if code != 0 || state == 0 {
		if state != 0 {
			f.encoderDestroy(state)
		}
		return nil, opusError(f, "opus_encoder_create", code)
	}
	return &Encoder{f: f, state: state, channels: channels}, nil
}

func (e *Encoder) EncodeFloat32(pcm []float32, packet []byte) (int, error) {
	if e == nil || e.state == 0 {
		return 0, ErrClosed
	}
	if len(pcm) == 0 || len(pcm)%e.channels != 0 || len(pcm)/e.channels > math.MaxInt32 ||
		len(packet) == 0 || len(packet) > math.MaxInt32 {
		return 0, errors.New("opus_encode_float: invalid PCM frame or packet buffer")
	}
	n := e.f.encodeFloat(e.state, &pcm[0], int32(len(pcm)/e.channels), &packet[0], int32(len(packet)))
	runtime.KeepAlive(pcm)
	runtime.KeepAlive(packet)
	if n < 0 {
		return 0, opusError(e.f, "opus_encode_float", n)
	}
	return int(n), nil
}

func (e *Encoder) Close() error {
	if e != nil && e.state != 0 {
		e.f.encoderDestroy(e.state)
		e.state = 0
	}
	return nil
}

type Decoder struct {
	f        *functions
	state    uintptr
	channels int
}

func NewDecoder(rate, channels int) (*Decoder, error) {
	if !validFormat(rate, channels) {
		return nil, fmt.Errorf("opus decoder: invalid rate %d or channel count %d", rate, channels)
	}
	f, err := getFunctions()
	if err != nil {
		return nil, err
	}
	var code int32
	state := f.decoderCreate(int32(rate), int32(channels), &code)
	if code != 0 || state == 0 {
		if state != 0 {
			f.decoderDestroy(state)
		}
		return nil, opusError(f, "opus_decoder_create", code)
	}
	return &Decoder{f: f, state: state, channels: channels}, nil
}

func (d *Decoder) DecodeFloat32(packet []byte, pcm []float32) (int, error) {
	if d == nil || d.state == 0 {
		return 0, ErrClosed
	}
	if len(pcm) == 0 || len(pcm)%d.channels != 0 || len(pcm)/d.channels > math.MaxInt32 ||
		len(packet) > math.MaxInt32 {
		return 0, errors.New("opus_decode_float: invalid PCM frame or packet buffer")
	}
	var data *byte // A nil packet requests packet loss concealment.
	if len(packet) != 0 {
		data = &packet[0]
	}
	n := d.f.decodeFloat(d.state, data, int32(len(packet)), &pcm[0], int32(len(pcm)/d.channels), 0)
	runtime.KeepAlive(packet)
	runtime.KeepAlive(pcm)
	if n < 0 {
		return 0, opusError(d.f, "opus_decode_float", n)
	}
	return int(n), nil
}

func (d *Decoder) Close() error {
	if d != nil && d.state != 0 {
		d.f.decoderDestroy(d.state)
		d.state = 0
	}
	return nil
}
