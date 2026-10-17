//go:build !safem_test_noaudio

package audio

import (
	"errors"

	"github.com/pion/opus"
)

var (
	errCodecClosed         = errors.New("opus codec is closed")
	errInvalidDecodeBuffer = errors.New("opus decode: invalid PCM frame buffer")
)

type opusFactory struct{}

func newDefaultCodecFactory() CodecFactory { return opusFactory{} }

type pionEncoder struct {
	enc    *opus.Encoder
	closed bool
}

func (opusFactory) NewEncoder(rate, channels int) (Encoder, error) {
	enc, err := opus.NewEncoder(
		opus.WithSampleRate(rate),
		opus.WithChannels(channels),
		opus.WithApplication(opus.ApplicationVoIP),
	)
	if err != nil {
		return nil, err
	}
	return &pionEncoder{enc: enc}, nil
}

func (e *pionEncoder) EncodeFloat32(pcm []float32, packet []byte) (int, error) {
	if e == nil || e.closed || e.enc == nil {
		return 0, errCodecClosed
	}
	return e.enc.EncodeFloat32(pcm, packet)
}

func (e *pionEncoder) Close() error {
	if e != nil {
		e.closed = true
		e.enc = nil
	}
	return nil
}

type pionDecoder struct {
	dec        opus.Decoder
	channels   int
	plcScratch []int16
	hasDecoded bool
	closed     bool
}

func (opusFactory) NewDecoder(rate, channels int) (Decoder, error) {
	dec, err := opus.NewDecoderWithOutput(rate, channels)
	if err != nil {
		return nil, err
	}
	return &pionDecoder{
		dec:        dec,
		channels:   channels,
		plcScratch: make([]int16, FrameSizeSamples*channels),
	}, nil
}

func (d *pionDecoder) DecodeFloat32(packet []byte, pcm []float32) (int, error) {
	if d == nil || d.closed {
		return 0, errCodecClosed
	}
	if len(pcm) == 0 || len(pcm)%d.channels != 0 {
		return 0, errInvalidDecodeBuffer
	}
	if len(packet) == 0 {
		frames := len(pcm) / d.channels
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
	if d != nil {
		d.closed = true
		d.plcScratch = nil
	}
	return nil
}