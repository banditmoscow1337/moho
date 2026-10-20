//go:build linux && (amd64 || arm64) && !safem_test_noaudio && cgo

package moho

// purego.NewCallback is unavailable in its Linux CGO implementation. The
// supported CGO-free build enumerates PulseAudio; a CGO build lists ALSA PCMs.
const pulseCallbackSupported = false
