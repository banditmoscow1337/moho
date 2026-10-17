package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"

	"github.com/banditmoscow1337/moho/voicebuf"
)

const (
	// SampleRate is the audio sampling rate in Hz (Opus Standard).
	SampleRate = 48000

	// Channels defines Mono audio (sufficient for voice).
	Channels = 1

	// FrameSizeMs is the duration of a single audio frame in milliseconds.
	// 20ms is a standard trade-off between latency and overhead.
	FrameSizeMs = 20

	// FrameSizeSamples is the number of samples per frame (960 @ 48kHz).
	FrameSizeSamples = SampleRate * FrameSizeMs / 1000

	// MaxEncodedSize is the buffer size for Opus packets (conservative upper bound).
	MaxEncodedSize = voicebuf.MaxEncodedSize

	// JitterBufferTarget is the target latency in frames.
	// 3 frames * 20ms = 60ms added latency to absorb network jitter.
	JitterBufferTarget = 3

	// JitterBufferWindow is the maximum number of packets held for one peer.
	// 32 frames at 20 ms gives a 640 ms bound, including reordered packets.
	JitterBufferWindow = 32
)

func releasePacket(p *[]byte) { voicebuf.Put(p) }

type jitterSlot struct {
	seq  uint16
	data *[]byte
}

// JitterStats counts rejected and evicted packets for one active peer.
type JitterStats struct {
	LatePackets      uint64
	DuplicatePackets uint64
	OverflowPackets  uint64
}

// JitterBuffer manages incoming audio packets for a single peer stream.
// It uses a standard sync.Mutex to protect the critical path, avoiding
// dangerous unsafe.Pointer swaps. At 50 packets/sec, mutex contention is negligible.
type JitterBuffer struct {
	mu       sync.Mutex
	decodeMu sync.Mutex

	// Slots are tagged to distinguish sequence numbers that share a ring index.
	slots [JitterBufferWindow]jitterSlot

	// count tracks the number of filled slots.
	count int

	latestSeq   uint16
	initialized bool

	// closed prevents new pushes during cleanup to avoid leaks.
	closed bool

	// Consumer state (Thread-local to the Audio Callback, protected by mu)
	lastPopped  uint16
	buffering   bool // True if we are filling the buffer (silence output)
	targetDepth int
	decoder     Decoder
	stats       JitterStats
}

// NewJitterBuffer creates a buffer with a specific target latency depth.
// The depth represents the number of frames to buffer before starting playback
// to handle network jitter.
func NewJitterBuffer(targetDepth int) *JitterBuffer {
	if targetDepth < 1 {
		targetDepth = 1
	} else if targetDepth > JitterBufferWindow {
		targetDepth = JitterBufferWindow
	}
	return &JitterBuffer{
		buffering:   true,
		targetDepth: targetDepth,
	}
}

// Level returns the approximate number of packets currently in the buffer.
// It is safe for concurrent calls.
func (jb *JitterBuffer) Level() int {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	return jb.count
}

func (jb *JitterBuffer) Stats() JitterStats {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	return jb.stats
}

// discardAll costs at most JitterBufferWindow operations, even after a large
// sequence jump. Callers hold mu.
func (jb *JitterBuffer) discardAll() {
	for i := range jb.slots {
		if p := jb.slots[i].data; p != nil {
			releasePacket(p)
			jb.slots[i] = jitterSlot{}
			jb.count--
		}
	}
}

// trimToHead discards packets skipped when playback catches up to the newest
// frame. All occupied slots are within one window of head.
func (jb *JitterBuffer) trimToHead(head uint16) {
	for i := range jb.slots {
		slot := &jb.slots[i]
		if slot.data != nil && int16(slot.seq-head) <= 0 {
			releasePacket(slot.data)
			*slot = jitterSlot{}
			jb.count--
			jb.stats.OverflowPackets++
		}
	}
	jb.lastPopped = head
}

// Push inserts a packet into the buffer based on its Sequence ID.
// It handles out-of-order arrival, duplicate detection, and protection against
// sequence wrapping.
//
// Push accepts ownership of the *[]byte data. If the packet
// is rejected (old/duplicate) or overwritten, Push handles freeing it back to the pool.
// This method is safe to call from multiple network routines concurrently.
func (jb *JitterBuffer) Push(seq uint16, data *[]byte) {
	if data == nil {
		return
	}
	jb.mu.Lock()
	defer jb.mu.Unlock()

	if jb.closed || len(*data) > voicebuf.PacketSize {
		releasePacket(data)
		return
	}
	if cap(*data) > voicebuf.PacketSize {
		// Public callers may provide a short view of a much larger network
		// allocation. Copy once rather than retaining that allocation in a
		// slot for the lifetime of the jitter buffer.
		small := voicebuf.Get()
		*small = (*small)[:len(*data)]
		copy(*small, *data)
		releasePacket(data)
		data = small
	}

	if !jb.initialized {
		jb.initialized = true
		jb.lastPopped = seq - 1
		jb.latestSeq = seq
	} else {
		ahead := int16(seq - jb.lastPopped)
		if ahead <= 0 {
			// Before the first playback frame, an earlier packet may arrive
			// after the initial packet. Extend the read head backwards within
			// the same bounded window so reordering at startup still works.
			behind := int16(jb.latestSeq - seq)
			if jb.buffering && behind >= 0 && behind < JitterBufferWindow {
				jb.lastPopped = seq - 1
			} else {
				jb.stats.LatePackets++
				releasePacket(data)
				return
			}
		}
		if ahead > JitterBufferWindow {
			// A gap or a producer running ahead of playback must not keep
			// stale audio. Reset in bounded time and rebuffer at the new seq.
			jb.stats.OverflowPackets += uint64(jb.count)
			jb.discardAll()
			jb.lastPopped = seq - 1
			jb.latestSeq = seq
			jb.buffering = true
		}
	}
	slot := &jb.slots[seq%JitterBufferWindow]
	if slot.data != nil {
		if slot.seq == seq {
			jb.stats.DuplicatePackets++
			releasePacket(data)
			return
		}
		// Ring index collision: evict the old frame, never overwrite its
		// owner silently.
		releasePacket(slot.data)
		jb.count--
		jb.stats.OverflowPackets++
	}
	*slot = jitterSlot{seq: seq, data: data}
	jb.count++
	if int16(seq-jb.latestSeq) > 0 {
		jb.latestSeq = seq
	}
}

// Pop retrieves the next sequential audio packet pointer for playback.
// It returns a *[]byte from the pool, or nil if the buffer is buffering/empty (underrun).
// The caller MUST call releasePacket() on the returned pointer after use.
//
// This method MUST ONLY be called by the single audio consumer thread (e.g., the audio callback).
func (jb *JitterBuffer) Pop() *[]byte {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	// Buffering State Logic
	// If we are in buffering mode, wait until we hit targetDepth.
	if jb.buffering {
		if jb.count >= jb.targetDepth {
			jb.buffering = false
			jb.trimToHead(jb.latestSeq - uint16(jb.targetDepth))
		} else {
			// Still buffering, output silence
			return nil
		}
	} else if jb.count > jb.targetDepth+8 {
		// Keep latency bounded when producer and consumer clocks drift.
		jb.trimToHead(jb.latestSeq - uint16(jb.targetDepth))
	}

	// Retrieve Next Packet
	next := jb.lastPopped + 1
	slot := &jb.slots[next%JitterBufferWindow]
	var ptr *[]byte
	if slot.seq == next {
		ptr = slot.data
		if ptr != nil {
			*slot = jitterSlot{}
			jb.count--
		}
	}
	jb.lastPopped = next

	if ptr != nil {
		if jb.count == 0 {
			jb.buffering = true
			jb.initialized = false
		}
		return ptr
	}

	// Underrun / Packet Loss
	// If buffer is completely empty, re-enter buffering state.
	if jb.count == 0 {
		jb.buffering = true
		jb.initialized = false
	}

	// Return nil triggers Packet Loss Concealment (PLC) in the Opus decoder.
	return nil
}

// Cleanup releases all buffered packets back to the pool and marks the buffer as closed.
// It must be called when removing a peer to prevent memory leaks of pooled buffers.
func (jb *JitterBuffer) Cleanup() {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	if jb.closed {
		return
	}
	jb.closed = true

	jb.discardAll()
	jb.decodeMu.Lock()
	if jb.decoder != nil {
		_ = jb.decoder.Close()
		jb.decoder = nil
	}
	jb.decodeMu.Unlock()
}

func (jb *JitterBuffer) decode(packet []byte, pcm []float32) (int, error) {
	jb.decodeMu.Lock()
	defer jb.decodeMu.Unlock()
	if jb.decoder == nil {
		return 0, errors.New("audio decoder closed")
	}
	return jb.decoder.DecodeFloat32(packet, pcm)
}

// Audio Engine

// Engine manages the audio hardware backend and the PCM processing worker.
type Engine struct {
	backend   Backend
	codecs    CodecFactory
	device    Device
	enc       Encoder
	pipeline  *audioPipeline
	running   bool
	controlMu sync.Mutex

	// sendFunc is the callback to push encoded packets to the network.
	sendFunc func([]byte)

	// inputs stores active JitterBuffers for mixing.
	// Key: PeerID
	// Value: *JitterBuffer
	// We use atomic.Value to implement Copy-On-Write for the processing worker.
	inputs atomic.Value

	// stateMu serializes updates to the inputs map (Add/Remove peer).
	// It is never held by the audio callback.
	stateMu sync.Mutex
	// The PCM worker holds this while processing a snapshot of inputs.
	// Removal publishes the new map, then waits before closing old decoders.
	mixMu sync.RWMutex

	sendSeq uint16

	// Hardware Device Selection
	inputID       DeviceID
	outputID      DeviceID
	timingEnabled bool
	generation    uint64

	// muted state: 0 = unmuted, 1 = muted
	muted int32
}

// NewEngine creates a new, idle audio engine with initialized internal state.
// It does not allocate hardware resources until Init() or Start() is called.
func NewEngine() *Engine {
	return NewEngineWithComponents(newDefaultBackend(), newDefaultCodecFactory())
}

// NewEngineWithComponents lets callers exercise the audio pipeline without
// opening a native audio device. Each Engine owns its backend and codec states.
func NewEngineWithComponents(backend Backend, codecs CodecFactory) *Engine {
	e := &Engine{backend: backend, codecs: codecs}
	// Initialize the atomic inputs map
	e.inputs.Store(make(map[string]*JitterBuffer))
	return e
}

// SetMute controls the microphone input state.
// If muted, the engine continues to process and send packets, but with silence (0.0),
// ensuring the connection remains alive (Keep-Alive via RTP/Opus).
func (e *Engine) SetMute(muted bool) {
	var val int32 = 0
	if muted {
		val = 1
	}
	atomic.StoreInt32(&e.muted, val)
}

// IsMuted returns the current mute status of the microphone.
// It is safe for concurrent use.
func (e *Engine) IsMuted() bool {
	return atomic.LoadInt32(&e.muted) == 1
}

// EnsureContext initializes the backend without opening a capture device.
func (e *Engine) EnsureContext() error {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	return e.backend.Init()
}

// ListDevices returns the available audio capture and playback devices found on the system.
func (e *Engine) ListDevices() ([]DeviceInfo, []DeviceInfo, error) {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	return e.backend.ListDevices()
}

// SetInputDevice configures the specific capture device to use by ID.
func (e *Engine) SetInputDevice(id DeviceID) {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	e.inputID = id
}

// SetOutputDevice configures the specific playback device to use by ID.
func (e *Engine) SetOutputDevice(id DeviceID) {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	e.outputID = id
}

// SelectedDevices returns the configured IDs, including the empty ID for
// each system default. It does not open an audio device.
func (e *Engine) SelectedDevices() (DeviceID, DeviceID) {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	return e.inputID, e.outputID
}

// SelectDevices applies both IDs together. An active call is restarted with
// the new endpoints; if that fails, the previous endpoints are restored and
// restarted so a failed device switch does not silently end the call.
func (e *Engine) SelectDevices(input, output DeviceID) error {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()

	previousInput, previousOutput := e.inputID, e.outputID
	running, send := e.running, e.sendFunc
	e.stopLocked()
	e.resetLocked()
	e.inputID, e.outputID = input, output
	if !running {
		return nil
	}
	if err := e.startLocked(send); err != nil {
		e.inputID, e.outputID = previousInput, previousOutput
		if restoreErr := e.startLocked(send); restoreErr != nil {
			return errors.Join(fmt.Errorf("switch audio devices: %w", err), fmt.Errorf("restore previous audio devices: %w", restoreErr))
		}
		return fmt.Errorf("switch audio devices: %w (previous devices restored)", err)
	}
	return nil
}

// Stats reports bounded-buffer overruns and playback silence since Start.
func (e *Engine) Stats() AudioStats {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	stats := AudioStats{}
	if e.pipeline != nil {
		stats = e.pipeline.stats()
	}
	stats.Generation, stats.Running = e.generation, e.running
	stats.Peers = len(e.inputs.Load().(map[string]*JitterBuffer))
	for _, jb := range e.inputs.Load().(map[string]*JitterBuffer) {
		jitter := jb.Stats()
		stats.JitterLate += jitter.LatePackets
		stats.JitterDuplicate += jitter.DuplicatePackets
		stats.JitterOverflow += jitter.OverflowPackets
	}
	return stats
}

// EnableTiming enables opt-in callback/worker timing for the next device.
// Call it before Init or Start so live callback configuration remains immutable.
func (e *Engine) EnableTiming() error {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	if e.device != nil {
		return errors.New("enable audio timing before device initialization")
	}
	e.timingEnabled = true
	return nil
}

// Init initializes the Opus encoder and prepares the hardware device configuration.
// It allocates the audio context but does not start the audio processing loop.
func (e *Engine) Init() error {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	return e.initLocked()
}

func (e *Engine) initLocked() error {
	if e.device != nil {
		return nil
	}
	var err error
	// VoIP application type optimizes for voice frequencies.
	if e.enc == nil {
		e.enc, err = e.codecs.NewEncoder(SampleRate, Channels)
		if err != nil {
			return fmt.Errorf("failed to create opus encoder: %w", err)
		}
	}

	if err := e.backend.Init(); err != nil {
		_ = e.enc.Close()
		e.enc = nil
		return err
	}

	config := DeviceConfig{
		InputID: e.inputID, OutputID: e.outputID,
		SampleRate: SampleRate, Channels: Channels, PeriodMillis: FrameSizeMs,
	}

	e.pipeline = newAudioPipeline(e)
	e.device, err = e.backend.Open(config, e.pipeline.callback)
	if err != nil {
		_ = e.enc.Close()
		e.enc = nil
		e.pipeline = nil
	}
	return err
}

// Start begins the PCM processing loop and starts hardware I/O. sendCallback
// runs on the worker and must copy the borrowed packet before returning.
func (e *Engine) Start(sendCallback func([]byte)) error {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	return e.startLocked(sendCallback)
}

func (e *Engine) startLocked(sendCallback func([]byte)) error {
	if e.running {
		return errors.New("audio engine already running")
	}
	e.sendFunc = sendCallback
	e.sendSeq = 0
	if e.device == nil {
		if err := e.initLocked(); err != nil {
			e.sendFunc = nil
			return err
		}
	}
	e.pipeline.start(sendCallback)
	if err := e.device.Start(); err != nil {
		e.pipeline.stop()
		e.sendFunc = nil
		e.resetLocked()
		return err
	}
	e.running = true
	e.generation++
	return nil
}

// Stop halts the audio processing loop and pauses hardware I/O.
func (e *Engine) Stop() {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	e.stopLocked()
}

func (e *Engine) stopLocked() {
	if e.running {
		_ = e.device.Stop() // Stop quiesces the hardware callback.
		e.running = false
	}
	if e.pipeline != nil {
		e.pipeline.stop()
	}
	e.sendFunc = nil
}

// Restart halts and re-initializes the engine, applying any new device configurations.
func (e *Engine) Restart() error {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	running := e.running
	cb := e.sendFunc
	e.stopLocked()
	e.resetLocked()
	if running {
		return e.startLocked(cb)
	}
	return nil
}

// Reset fully tears down the device handle and uninitializes the hardware interface.
func (e *Engine) Reset() {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	e.stopLocked()
	e.resetLocked()
}

func (e *Engine) resetLocked() {
	if e.device != nil {
		_ = e.device.Close()
		e.device = nil
	}
	e.pipeline = nil
	if e.enc != nil {
		_ = e.enc.Close()
		e.enc = nil
	}
}

// Cleanup stops the engine and frees all native resources (Context, Encoder).
func (e *Engine) Cleanup() {
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	e.stopLocked()
	e.resetLocked()
	e.RemoveAllPeers()
	_ = e.backend.Close()
}

// AddPeer creates a new JitterBuffer for the specified peer ID and adds them to the mix.
// It uses a Copy-On-Write strategy to safely update the active inputs list without
// blocking the hardware callback.
func (e *Engine) AddPeer(id string) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	oldMap := e.inputs.Load().(map[string]*JitterBuffer)

	// Check if already exists
	if _, exists := oldMap[id]; exists {
		return nil
	}

	decoder, err := e.codecs.NewDecoder(SampleRate, Channels)
	if err != nil {
		return err
	}

	// Clone to new map
	newMap := make(map[string]*JitterBuffer, len(oldMap)+1)
	maps.Copy(newMap, oldMap)

	// Add new peer
	jb := NewJitterBuffer(JitterBufferTarget)
	jb.decoder = decoder
	newMap[id] = jb

	// Atomically Swap
	e.inputs.Store(newMap)
	return nil
}

// RemovePeer removes the specified peer from the audio mix and cleans up their resources.
// It uses Copy-On-Write for thread safety and ensures the JitterBuffer is drained.
func (e *Engine) RemovePeer(id string) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	oldMap := e.inputs.Load().(map[string]*JitterBuffer)

	removed, exists := oldMap[id]
	if !exists {
		return
	}

	// Clone and Delete
	newMap := make(map[string]*JitterBuffer, len(oldMap))
	for k, v := range oldMap {
		if k != id {
			newMap[k] = v
		}
	}

	e.inputs.Store(newMap)
	e.mixMu.Lock()
	removed.Cleanup()
	e.mixMu.Unlock()
}

// RemoveAllPeers clears all active peers from the engine and cleans up their buffers.
// This is typically called when a call ends to reset the mixing state.
func (e *Engine) RemoveAllPeers() {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	oldMap := e.inputs.Load().(map[string]*JitterBuffer)
	e.inputs.Store(make(map[string]*JitterBuffer))
	e.mixMu.Lock()
	for _, v := range oldMap {
		v.Cleanup()
	}
	e.mixMu.Unlock()
}

// ProcessIncoming routes an incoming audio packet to the appropriate peer's JitterBuffer.
//
// This method accepts ownership of the data pointer. If the peer is unknown
// or the data is invalid, it frees the packet immediately back to the pool.
// It is designed to be called directly from the network ingress layer.
func (e *Engine) ProcessIncoming(sourceID string, data *[]byte) {
	if data == nil {
		return
	}

	if len(*data) < 3 || len(*data) > voicebuf.PacketSize {
		releasePacket(data)
		return
	}
	// Parse Sequence ID from first 2 bytes
	seq := binary.BigEndian.Uint16((*data)[0:2])

	// Keep the original base slice for the shared pool. Only the decoder
	// receives a payload subslice; advancing *data would retain a truncated
	// backing view and prevent safe reuse of the original allocation.

	// Lock-free read for network thread
	currentInputs := e.inputs.Load().(map[string]*JitterBuffer)

	if jb, exists := currentInputs[sourceID]; exists {
		// Pass ownership to JitterBuffer
		jb.Push(seq, data)
	} else {
		// Peer not active in audio engine, drop packet to prevent leak
		releasePacket(data)
	}
}
