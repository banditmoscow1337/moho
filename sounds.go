package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
)

const (
	soundOutputRate  = 48000
	soundChunk       = 65536
	soundStreamAhead = soundChunk * 4
)

// VoiceID identifies a particular playback, even after its voice slot is reused.
// Zero means that a sound was declined because all eligible voices had higher priority.
type VoiceID uint64

// PCMFormat describes interleaved, little-endian PCM. Eight-bit samples are unsigned.
type PCMFormat struct {
	Channels      int
	BitsPerSample int
	SampleRate    int
}

func (f PCMFormat) frameBytes() (int, error) {
	if (f.Channels != 1 && f.Channels != 2) ||
		(f.BitsPerSample != 8 && f.BitsPerSample != 16) ||
		f.SampleRate < 1 || f.SampleRate > 384000 {
		return 0, fmt.Errorf("unsupported PCM format %+v", f)
	}
	return f.Channels * f.BitsPerSample / 8, nil
}

// Sound is a static PCM sound or a stream that supplies PCM through PCMOpener.
// Keep its fields and the PCM bytes unchanged while using it with a Driver.
// Distinct static sounds must have distinct names: preloaded data is cached by Name.
type Sound struct {
	Name       string
	Category   string
	Format     PCMFormat
	PCM        []byte
	Streaming  bool
	BaseVolume float64
	PCMOpener  func() (io.ReadCloser, error)
}

// OpenPCM opens a new reader at the beginning of a streaming sound.
func (s *Sound) OpenPCM() (io.ReadCloser, error) {
	if s == nil || s.PCMOpener == nil {
		return nil, errors.New("streaming sound has no PCM opener")
	}
	reader, err := s.PCMOpener()
	if err == nil && reader == nil {
		return nil, errors.New("PCM opener returned a nil reader")
	}
	return reader, err
}

type soundVoice struct {
	id, started                                   VoiceID
	name, category                                string
	priority, baseVolume                          float64
	sound                                         *Sound
	format                                        PCMFormat
	width                                         int
	pcm                                           []byte
	streamWake                                    chan struct{}
	streamStop                                    chan struct{}
	streaming, streamEOF, loop, duringPause, done bool
	pos                                           float64 // source frames, including the fractional resampling position
}

// Driver plays game sounds on moho's native playback device without OpenAL.
// Call Update regularly (for example once per game tick) to release naturally
// completed voices and report stream errors. Streaming I/O runs on workers;
// neither Update nor the device callback reads files.
type Driver struct {
	controlMu  sync.Mutex // serializes public operations
	mu         sync.Mutex // protects the voices from the device callback
	backend    Backend
	device     Device
	voices     []soundVoice
	cache      map[string]cachedPCM
	master     float64
	serial     VoiceID
	paused     bool
	closed     bool
	pendingErr error
	workers    sync.WaitGroup
}

type cachedPCM struct {
	format PCMFormat
	data   []byte
}

// New opens a playback-only device with a bounded number of voices.
func New(maxVoices int) (*Driver, error) {
	return NewDriverWithBackend(maxVoices, newDefaultBackend())
}

// NewDriverWithBackend uses a caller-supplied backend. Driver takes ownership
// of it and closes it on Close or when device initialization fails.
func NewDriverWithBackend(maxVoices int, backend Backend) (*Driver, error) {
	if maxVoices < 1 || maxVoices > 128 {
		return nil, errors.New("voice pool must be between 1 and 128")
	}
	if backend == nil {
		return nil, errors.New("audio backend is nil")
	}
	d := &Driver{backend: backend, voices: make([]soundVoice, maxVoices), cache: make(map[string]cachedPCM), master: 1}
	if err := backend.Init(); err != nil {
		_ = backend.Close()
		return nil, err
	}
	device, err := backend.Open(DeviceConfig{
		SampleRate: soundOutputRate, Channels: 2, PeriodMillis: 20, PlaybackOnly: true,
	}, d.callback)
	if err != nil {
		_ = backend.Close()
		return nil, err
	}
	if device == nil {
		_ = backend.Close()
		return nil, errors.New("audio backend returned a nil device")
	}
	d.device = device
	if err := device.Start(); err != nil {
		_ = device.Close()
		_ = backend.Close()
		return nil, err
	}
	return d, nil
}

func (d *Driver) staticPCM(sound *Sound) ([]byte, error) {
	if cached, ok := d.cache[sound.Name]; ok {
		if cached.format != sound.Format {
			return nil, fmt.Errorf("sound %s: cached PCM format differs", sound.Name)
		}
		return cached.data, nil
	}
	width, err := sound.Format.frameBytes()
	if err != nil {
		return nil, err
	}
	if len(sound.PCM) == 0 || len(sound.PCM)%width != 0 {
		return nil, fmt.Errorf("sound %s: invalid PCM buffer size", sound.Name)
	}
	pcm := append([]byte(nil), sound.PCM...)
	d.cache[sound.Name] = cachedPCM{format: sound.Format, data: pcm}
	return pcm, nil
}

// Preload validates and caches a static sound by name.
func (d *Driver) Preload(sound *Sound) error {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if d.closed || sound == nil || sound.Streaming {
		return errors.New("cannot preload closed driver or streaming sound")
	}
	_, err := d.staticPCM(sound)
	return err
}

// Play starts a voice, or returns the existing voice for a looping sound name.
// A low-priority request may return ID zero and a nil error. Streaming sources
// open asynchronously; their I/O errors are returned by Update.
func (d *Driver) Play(sound *Sound, priority float64, loop bool) (VoiceID, error) {
	return d.play(sound, priority, loop, false)
}

// PlayDuringPause starts a sound while PauseAll is in effect.
func (d *Driver) PlayDuringPause(sound *Sound, priority float64, loop bool) (VoiceID, error) {
	return d.play(sound, priority, loop, true)
}

func (d *Driver) play(sound *Sound, priority float64, loop, duringPause bool) (VoiceID, error) {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if d.closed {
		return 0, errors.New("audio is closed")
	}
	if sound == nil || math.IsNaN(priority) || math.IsNaN(sound.BaseVolume) ||
		math.IsInf(sound.BaseVolume, 0) || sound.BaseVolume < 0 {
		return 0, errors.New("invalid sound or priority")
	}
	if sound.Streaming && sound.PCMOpener == nil {
		return 0, errors.New("streaming sound has no PCM opener")
	}
	width, err := sound.Format.frameBytes()
	if err != nil {
		return 0, err
	}
	if loop {
		d.mu.Lock()
		for i := range d.voices {
			v := &d.voices[i]
			if v.id != 0 && v.name == sound.Name && v.loop {
				if duringPause && d.paused {
					v.duringPause = true
				}
				id := v.id
				d.mu.Unlock()
				return id, nil
			}
		}
		d.mu.Unlock()
	}
	if err := d.updateLocked(); err != nil {
		return 0, err
	}
	d.mu.Lock()
	slot := -1
	for i := range d.voices {
		if d.voices[i].id == 0 {
			slot = i
			break
		}
	}
	if slot < 0 {
		for i := range d.voices {
			v := &d.voices[i]
			if v.loop && v.category == "music" {
				continue
			}
			if slot < 0 || v.priority < d.voices[slot].priority ||
				(v.priority == d.voices[slot].priority && v.started < d.voices[slot].started) {
				slot = i
			}
		}
		if slot < 0 || priority < d.voices[slot].priority {
			d.mu.Unlock()
			return 0, nil
		}
	}
	d.mu.Unlock()
	if err := d.releaseLocked(&d.voices[slot]); err != nil {
		return 0, err
	}
	var pcm []byte
	if !sound.Streaming {
		pcm, err = d.staticPCM(sound)
		if err != nil {
			return 0, err
		}
	}
	d.serial++
	if d.serial == 0 {
		d.serial++
	}
	v := &d.voices[slot]
	var wake, stop chan struct{}
	if sound.Streaming {
		wake, stop = make(chan struct{}, 1), make(chan struct{})
	}
	d.mu.Lock()
	*v = soundVoice{
		id: d.serial, started: d.serial, name: sound.Name, category: sound.Category,
		priority: priority, baseVolume: sound.BaseVolume, sound: sound, format: sound.Format, width: width,
		pcm: pcm, streamWake: wake, streamStop: stop, streaming: sound.Streaming,
		loop: loop, duringPause: duringPause && d.paused,
	}
	d.mu.Unlock()
	if sound.Streaming {
		d.workers.Add(1)
		go d.feedStream(v, d.serial, sound, width, loop, wake, stop)
	}
	return d.serial, nil
}

// feedStream owns the reader and decodes ahead without holding either driver
// mutex during I/O. A reused voice slot is identified by its unique ID.
func (d *Driver) feedStream(v *soundVoice, id VoiceID, sound *Sound, width int, loop bool, wake, stop <-chan struct{}) {
	defer d.workers.Done()
	var stream io.ReadCloser
	var streamErr error
	defer func() {
		if stream != nil {
			streamErr = errors.Join(streamErr, stream.Close())
		}
		if streamErr != nil {
			d.mu.Lock()
			if v.id == id {
				if d.pendingErr == nil {
					d.pendingErr = streamErr
				}
				v.done = true
			}
			d.mu.Unlock()
		}
	}()

	bytesSinceOpen := 0
	for {
		d.mu.Lock()
		active := v.id == id && v.streaming && !v.done
		queued := len(v.pcm)
		d.mu.Unlock()
		if !active {
			return
		}
		if queued >= soundStreamAhead {
			select {
			case <-wake:
			case <-stop:
				return
			}
			continue
		}
		select {
		case <-stop:
			return
		default:
		}
		if stream == nil {
			var err error
			stream, err = sound.OpenPCM()
			if err != nil {
				streamErr = err
				return
			}
			bytesSinceOpen = 0
		}
		data := make([]byte, soundChunk)
		n, err := io.ReadFull(stream, data)
		if n%width != 0 {
			streamErr = fmt.Errorf("sound %s: incomplete PCM frame", sound.Name)
			return
		}
		if n > 0 {
			d.mu.Lock()
			if v.id != id || !v.streaming || v.done {
				d.mu.Unlock()
				return
			}
			v.pcm = append(v.pcm, data[:n]...)
			d.mu.Unlock()
			bytesSinceOpen += n
		}
		if err == nil {
			continue
		}
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			streamErr = fmt.Errorf("sound %s: read PCM: %w", sound.Name, err)
			return
		}
		if !loop {
			d.mu.Lock()
			if v.id == id {
				v.streamEOF = true
				if len(v.pcm) == 0 {
					v.done = true
					streamErr = fmt.Errorf("sound %s: empty PCM stream", sound.Name)
				}
			}
			d.mu.Unlock()
			return
		}
		if bytesSinceOpen == 0 {
			streamErr = fmt.Errorf("sound %s: empty PCM stream", sound.Name)
			return
		}
		// Once a tiny looping stream is fully buffered, use the static
		// playback path so its wrap does not depend on worker scheduling.
		d.mu.Lock()
		smallLoop := v.id == id && bytesSinceOpen <= soundChunk &&
			len(v.pcm) == bytesSinceOpen && v.pos == 0
		if smallLoop {
			v.streaming = false
		}
		d.mu.Unlock()
		if smallLoop {
			return
		}
		if streamErr = stream.Close(); streamErr != nil {
			stream = nil
			return
		}
		stream = nil
	}
}

// Update reclaims completed voices and reports asynchronous stream errors.
func (d *Driver) Update() error {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	return d.updateLocked()
}

func (d *Driver) updateLocked() error {
	if d.closed {
		return nil
	}
	d.mu.Lock()
	pendingErr := d.pendingErr
	d.pendingErr = nil
	d.mu.Unlock()
	if pendingErr != nil {
		return pendingErr
	}
	for i := range d.voices {
		v := &d.voices[i]
		d.mu.Lock()
		done := v.done
		d.mu.Unlock()
		if done {
			if err := d.releaseLocked(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Driver) releaseLocked(v *soundVoice) error {
	d.mu.Lock()
	stop := v.streamStop
	*v = soundVoice{}
	d.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	return nil
}

func (d *Driver) record(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil && d.pendingErr == nil {
		d.pendingErr = err
	}
}

// StopVoice stops a single playback; unknown IDs have no effect.
func (d *Driver) StopVoice(id VoiceID) {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if d.closed || id == 0 {
		return
	}
	for i := range d.voices {
		if d.voices[i].id == id {
			d.record(d.releaseLocked(&d.voices[i]))
			return
		}
	}
}

// StopSound stops all voices with the given sound name.
func (d *Driver) StopSound(name string) {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if d.closed {
		return
	}
	for i := range d.voices {
		if d.voices[i].id != 0 && d.voices[i].name == name {
			d.record(d.releaseLocked(&d.voices[i]))
		}
	}
}

// StopAll stops every active voice.
func (d *Driver) StopAll() {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if d.closed {
		return
	}
	for i := range d.voices {
		d.record(d.releaseLocked(&d.voices[i]))
	}
}

// PauseAll suspends existing voices and starts made by Play.
func (d *Driver) PauseAll() {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.paused {
		return
	}
	d.paused = true
	for i := range d.voices {
		d.voices[i].duringPause = false
	}
}

// ResumeAll resumes voices suspended by PauseAll.
func (d *Driver) ResumeAll() {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	d.mu.Lock()
	d.paused = false
	d.mu.Unlock()
}

// SetMasterGain sets the gain of every voice, from silence at 0 to full at 1.
func (d *Driver) SetMasterGain(gain float64) error {
	if gain < 0 || gain > 1 || math.IsNaN(gain) {
		return errors.New("audio gain must be in [0,1]")
	}
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	d.mu.Lock()
	d.master = gain
	d.mu.Unlock()
	return nil
}

// Close stops playback, closes streams and releases the native backend.
func (d *Driver) Close() error {
	if d == nil {
		return nil
	}
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if d.closed {
		return nil
	}
	// Stop waits for the final callback before closing the streams.
	var result error
	if d.device != nil {
		result = errors.Join(result, d.device.Stop(), d.device.Close())
	}
	for i := range d.voices {
		result = errors.Join(result, d.releaseLocked(&d.voices[i]))
	}
	d.workers.Wait()
	if d.backend != nil {
		result = errors.Join(result, d.backend.Close())
	}
	d.mu.Lock()
	d.closed = true
	d.cache = nil
	result = errors.Join(result, d.pendingErr)
	d.mu.Unlock()
	return result
}

// callback mixes directly into the backend's little-endian float32 output.
// It holds no stream I/O or user callback and allocates no per-frame storage.
func (d *Driver) callback(output, _ []byte, frameCount uint32) {
	clear(output)
	frames := min(len(output)/8, int(frameCount))
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	for vi := range d.voices {
		v := &d.voices[vi]
		if v.id == 0 || v.done || d.paused && !v.duringPause {
			continue
		}
		gain := float32(d.master * v.baseVolume)
		for frame := 0; frame < frames; frame++ {
			left, right, ok := v.nextFrame()
			if !ok {
				break
			}
			i := frame * 8
			left = math.Float32frombits(binary.LittleEndian.Uint32(output[i:])) + left*gain
			right = math.Float32frombits(binary.LittleEndian.Uint32(output[i+4:])) + right*gain
			binary.LittleEndian.PutUint32(output[i:], math.Float32bits(left))
			binary.LittleEndian.PutUint32(output[i+4:], math.Float32bits(right))
		}
		if v.streaming {
			drop := min(int(v.pos), len(v.pcm)/v.width)
			v.pcm = v.pcm[drop*v.width:]
			v.pos -= float64(drop)
			if v.streamEOF && len(v.pcm) == 0 {
				v.done = true
			} else if len(v.pcm) < soundStreamAhead {
				select {
				case v.streamWake <- struct{}{}:
				default:
				}
			}
		}
	}
	for frame := 0; frame < frames; frame++ {
		for channel := 0; channel < 2; channel++ {
			i := frame*8 + channel*4
			sample := math.Float32frombits(binary.LittleEndian.Uint32(output[i:]))
			binary.LittleEndian.PutUint32(output[i:], math.Float32bits(max(-1, min(1, sample))))
		}
	}
}

func (v *soundVoice) nextFrame() (float32, float32, bool) {
	frames := len(v.pcm) / v.width
	if frames == 0 {
		if v.streamEOF {
			v.done = true
		}
		return 0, 0, false
	}
	if v.loop && !v.streaming && v.pos >= float64(frames) {
		v.pos = math.Mod(v.pos, float64(frames))
	}
	if v.pos >= float64(frames) {
		if !v.streaming || v.streamEOF {
			v.done = true
		}
		return 0, 0, false
	}
	index := int(v.pos)
	next := index + 1
	if next >= frames {
		if v.loop && !v.streaming {
			next = 0
		} else {
			next = index
		}
	}
	l0, r0 := v.sample(index)
	l1, r1 := v.sample(next)
	frac := float32(v.pos - float64(index))
	v.pos += float64(v.format.SampleRate) / soundOutputRate
	return l0 + (l1-l0)*frac, r0 + (r1-r0)*frac, true
}

func (v *soundVoice) sample(frame int) (float32, float32) {
	index := frame * v.width
	decode := func(i int) float32 {
		if v.format.BitsPerSample == 8 {
			return (float32(v.pcm[i]) - 128) / 128
		}
		return float32(int16(binary.LittleEndian.Uint16(v.pcm[i:]))) / 32768
	}
	left := decode(index)
	if v.format.Channels == 1 {
		return left, left
	}
	return left, decode(index + v.format.BitsPerSample/8)
}
