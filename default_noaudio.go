//go:build safem_test_noaudio

package audio

import "errors"

var errAudioDisabled = errors.New("audio disabled by safem_test_noaudio")

type disabledBackend struct{}
type disabledCodecFactory struct{}

func newDefaultBackend() Backend           { return disabledBackend{} }
func newDefaultCodecFactory() CodecFactory { return disabledCodecFactory{} }

func (disabledBackend) Init() error { return errAudioDisabled }
func (disabledBackend) ListDevices() ([]DeviceInfo, []DeviceInfo, error) {
	return nil, nil, errAudioDisabled
}
func (disabledBackend) Open(DeviceConfig, DataCallback) (Device, error) {
	return nil, errAudioDisabled
}
func (disabledBackend) Close() error { return nil }

func (disabledCodecFactory) NewEncoder(int, int) (Encoder, error) {
	return nil, errAudioDisabled
}
func (disabledCodecFactory) NewDecoder(int, int) (Decoder, error) {
	return nil, errAudioDisabled
}
