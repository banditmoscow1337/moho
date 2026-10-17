//go:build !safem_test_noaudio

package audio

import "github.com/banditmoscow1337/moho/opuscodec"

type opusFactory struct{}

func newDefaultCodecFactory() CodecFactory { return opusFactory{} }

func (opusFactory) NewEncoder(rate, channels int) (Encoder, error) {
	return opuscodec.NewEncoder(rate, channels)
}

func (opusFactory) NewDecoder(rate, channels int) (Decoder, error) {
	return opuscodec.NewDecoder(rate, channels)
}
