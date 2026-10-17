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

## Game sounds and music

`Driver` provides the playback operations used by game sound effects and music.
It opens a playback-only native device; no microphone or OpenAL installation is
needed. It accepts mono or stereo, unsigned 8-bit or little-endian signed 16-bit
PCM. Input rates are converted to the 48 kHz stereo output.

```go
driver, err := audio.New(32)
if err != nil {
	return err
}
defer driver.Close()

effect := &audio.Sound{
	Name: "hit", Category: "effects",
	Format: audio.PCMFormat{Channels: 1, BitsPerSample: 16, SampleRate: 44100},
	PCM: decodedPCM, BaseVolume: 0.8,
}
if err := driver.Preload(effect); err != nil {
	return err
}
_, err = driver.Play(effect, 1, false)
```

Call `driver.Update()` once per game tick. It refills streaming sounds outside
the audio callback and releases completed voices. To stream music, set
`Streaming: true` and supply `PCMOpener: func() (io.ReadCloser, error)` that
opens the PCM data from the start on each call. A looping stream reopens that
reader when it reaches the end. `PlayDuringPause`, `StopVoice`, `StopSound`,
`StopAll`, `PauseAll`, `ResumeAll`, and `SetMasterGain` mirror the game
driver's controls. Looping sounds reuse a voice by name; looping sounds in the
`"music"` category are protected when the voice pool is full. Keep `Sound`
fields immutable while a driver uses them, and use a unique name for each
static sound.

If your game already has a sound type, create an `audio.Sound` from its PCM,
format and metadata. Assign its existing `OpenPCM` method to `PCMOpener` for
streaming sounds. A game interface using its own `Sound` and `VoiceID` types
needs an adapter for those types. `NewDriverWithBackend` accepts a custom
backend for tests or alternate output devices.

## Checks

```sh
CGO_ENABLED=0 go test -tags safem_test_noaudio ./...
CGO_ENABLED=0 go test ./...
CGO_ENABLED=1 go test -race -tags safem_test_noaudio ./...
```

The `safem_test_noaudio` build tag retains compatibility with SAFEM's existing
hardware-free test setup. Native integration tests are opt-in using
`SAFEM_REQUIRE_NATIVE_AUDIO_TEST=1` or `SAFEM_REQUIRE_OPUS_TEST=1` on a machine
with the required runtime libraries.

## License

[MIT](LICENSE)
