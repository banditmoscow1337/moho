package audio

// DeviceID is an opaque identifier returned by a backend. The empty ID selects
// the system default. IDs are valid only for the backend that returned them.
type DeviceID string

type DeviceInfo struct {
	ID   DeviceID
	Name string
}

// DeviceConfig describes the PCM format requested from a backend. Buffers in
// DataCallback are interleaved float32 PCM in little-endian byte order.
type DeviceConfig struct {
	InputID      DeviceID
	OutputID     DeviceID
	SampleRate   uint32
	Channels     uint32
	PeriodMillis uint32
}

// DataCallback's buffers are borrowed until the callback returns. frameCount
// may differ from the requested period and must not be assumed to be an Opus
// frame. Callbacks from one device must be serialized. The backend must stop
// callbacks before Device.Stop or Device.Close returns; the rings may be reset
// immediately after Stop.
type DataCallback func(output, input []byte, frameCount uint32)

type Device interface {
	Start() error
	Stop() error
	Close() error
}

// Backend owns the device inventory and any native audio context. Close must
// be called after all devices it created have been closed.
type Backend interface {
	Init() error
	ListDevices() (capture, playback []DeviceInfo, err error)
	Open(DeviceConfig, DataCallback) (Device, error)
	Close() error
}

// Encoder and Decoder belong to one stream each. They are never called
// concurrently. Close releases any resources owned by the implementation.
type Encoder interface {
	EncodeFloat32(pcm []float32, packet []byte) (int, error)
	Close() error
}

type Decoder interface {
	DecodeFloat32(packet []byte, pcm []float32) (int, error)
	Close() error
}

type CodecFactory interface {
	NewEncoder(sampleRate, channels int) (Encoder, error)
	NewDecoder(sampleRate, channels int) (Decoder, error)
}
