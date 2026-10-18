//go:build !safem_test_noaudio

package audio

import (
	"errors"
	"sync"

	"github.com/pion/opus"
)

var (
	errCodecClosed         = errors.New("opus codec is closed")
	errInvalidDecodeBuffer = errors.New("opus decode: invalid PCM frame buffer")
)

type codecKey struct {
	rate, channels int
}

var (
	encoderPools sync.Map // map[codecKey]*sync.Pool
	decoderPools sync.Map // map[codecKey]*sync.Pool
)

var (
	encPool48kMono   sync.Pool
	encPool48kStereo sync.Pool
	decPool48kMono   sync.Pool
	decPool48kStereo sync.Pool
)

func getEncoderPool(k codecKey) *sync.Pool {
	if k.rate == SampleRate {
		switch k.channels {
		case 1:
			return &encPool48kMono
		case 2:
			return &encPool48kStereo
		}
	}
	if p, ok := encoderPools.Load(k); ok {
		return p.(*sync.Pool)
	}
	p := &sync.Pool{}
	actual, _ := encoderPools.LoadOrStore(k, p)
	return actual.(*sync.Pool)
}

func getDecoderPool(k codecKey) *sync.Pool {
	if p, ok := decoderPools.Load(k); ok {
		return p.(*sync.Pool)
	}
	p := &sync.Pool{}
	actual, _ := decoderPools.LoadOrStore(k, p)
	return actual.(*sync.Pool)
}

type opusFactory struct{}

func newDefaultCodecFactory() CodecFactory { return opusFactory{} }

type pionEncoder struct {
	enc    *opus.Encoder
	key    codecKey
	closed bool
}

func (opusFactory) NewEncoder(rate, channels int) (Encoder, error) {
	k := codecKey{rate: rate, channels: channels}
	if v := getEncoderPool(k).Get(); v != nil {
		e := v.(*pionEncoder)
		e.closed = false
		return e, nil
	}
	enc, err := opus.NewEncoder(
		opus.WithSampleRate(rate),
		opus.WithChannels(channels),
		opus.WithApplication(opus.ApplicationVoIP),
	)
	if err != nil {
		return nil, err
	}
	return &pionEncoder{enc: enc, key: k}, nil
}

func (e *pionEncoder) EncodeFloat32(pcm []float32, packet []byte) (int, error) {
	if e == nil || e.closed || e.enc == nil {
		return 0, errCodecClosed
	}
	return e.enc.EncodeFloat32(pcm, packet)
}

func (e *pionEncoder) Close() error {
	if e != nil && !e.closed {
		e.closed = true
		getEncoderPool(e.key).Put(e)
	}
	return nil
}

type pionDecoder struct {
	dec        opus.Decoder
	key        codecKey
	plcScratch []int16
	hasDecoded bool
	closed     bool
}

func (opusFactory) NewDecoder(rate, channels int) (Decoder, error) {
	k := codecKey{rate: rate, channels: channels}
	if v := getDecoderPool(k).Get(); v != nil {
		d := v.(*pionDecoder)
		d.hasDecoded = false
		d.closed = false
		return d, nil
	}
	dec, err := opus.NewDecoderWithOutput(rate, channels)
	if err != nil {
		return nil, err
	}
	return &pionDecoder{
		dec:        dec,
		key:        k,
		plcScratch: make([]int16, FrameSizeSamples*channels),
	}, nil
}

func (d *pionDecoder) DecodeFloat32(packet []byte, pcm []float32) (int, error) {
	if d == nil || d.closed {
		return 0, errCodecClosed
	}
	if len(pcm) == 0 || len(pcm)%d.key.channels != 0 {
		return 0, errInvalidDecodeBuffer
	}
	if len(packet) == 0 {
		frames := len(pcm) / d.key.channels
		if !d.hasDecoded {
			clear(pcm)
			return frames, nil
		}
		if len(d.plcScratch) < len(pcm) {
			d.plcScratch = make([]int16, len(pcm))
		}
		buf := d.plcScratch[:len(pcm)]
		if err := d.dec.DecodePLC(buf); err != nil {
			return 0, err
		}
		for i, s := range buf {
			pcm[i] = float32(s) / 32768
		}
		return frames, nil
	}
	n, err := d.dec.DecodeToFloat32(packet, pcm)
	if err != nil {
		return 0, err
	}
	d.hasDecoded = true
	return n, nil
}

func (d *pionDecoder) Close() error {
	if d != nil && !d.closed {
		d.closed = true
		getDecoderPool(d.key).Put(d)
	}
	return nil
}