//go:build linux && (amd64 || arm64) && !safem_test_noaudio && !cgo

package moho

const pulseCallbackSupported = true
