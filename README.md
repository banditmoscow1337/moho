# moho

`moho` is the Go audio library extracted from SAFEM's `protocol/audio` package.
It provides a voice engine, jitter buffer, native input/output backends for
Linux, macOS and Windows, and a dynamically loaded Opus codec. The root Go
package is named `audio`; `opuscodec` and `voicebuf` are subpackages.

## Install

Use Go 1.27 or later. This repository is private, so configure GitHub read
access and `GOPRIVATE=github.com/banditmoscow1337/moho` before downloading it.
For example, after configuring Git credentials:

```sh
GOPRIVATE=github.com/banditmoscow1337/moho go get github.com/banditmoscow1337/moho@v0.1.0
```

The codec loads the system's libopus at runtime. Native capture and playback
use PulseAudio or ALSA on Linux, Core Audio on macOS and WASAPI on Windows.
The library builds without CGO on supported `amd64` and `arm64` targets.
`SAFEM_OPUS_LIBRARY` may point to an absolute path for a particular libopus
build. A missing codec or audio backend is reported when the device or codec
is initialized.

```go
import audio "github.com/banditmoscow1337/moho"

engine := audio.NewEngine()
defer engine.Cleanup()
```

`NewEngineWithComponents` accepts custom backends and codecs for applications
and tests. Packet buffers are pooled by `github.com/banditmoscow1337/moho/voicebuf`.
An application that feeds voice packets to `Engine` should acquire and release
buffers using that package.

## Checks

```sh
CGO_ENABLED=0 go test -tags safem_test_noaudio ./...
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go test -race -tags safem_test_noaudio ./...
```

The `safem_test_noaudio` build tag retains compatibility with SAFEM's existing
hardware-free test setup. Native integration tests are opt-in using
`SAFEM_REQUIRE_NATIVE_AUDIO_TEST=1` or `SAFEM_REQUIRE_OPUS_TEST=1` on a machine
with the required runtime libraries.

## License

[MIT](LICENSE)
