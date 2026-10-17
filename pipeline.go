package audio

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const frameBytes = FrameSizeSamples * Channels * 4

// AudioStats includes ring loss in samples and codec/jitter errors in packets.
// Pipeline counters reset on Start; Generation distinguishes successful starts.
// Jitter counters belong to peer buffers and disappear when peers are removed.
type AudioStats struct {
	Generation      uint64
	Running         bool
	Peers           int
	CaptureQueued   int
	PlaybackQueued  int
	TimingEnabled   bool
	CallbackTiming  TimingStats
	WorkerTiming    TimingStats
	EncodeErrors    uint64
	DecodeErrors    uint64
	CaptureDropped  uint64
	PlaybackDropped uint64
	PlaybackSilence uint64
	JitterLate      uint64
	JitterDuplicate uint64
	JitterOverflow  uint64
}

// audioPipeline owns two single-producer/single-consumer rings. The hardware
// callback only copies PCM and signals the worker; all codec and peer work
// happens on the worker goroutine.
type audioPipeline struct {
	engine   *Engine
	capture  pcmRing // callback writes, worker reads
	playback pcmRing // worker writes, callback reads
	wake     chan struct{}
	quit     chan struct{}
	wg       sync.WaitGroup

	captureDropped  atomic.Uint64
	playbackDropped atomic.Uint64
	playbackSilence atomic.Uint64
	encodeErrors    atomic.Uint64
	decodeErrors    atomic.Uint64
	timing          bool
	callbackTiming  durationCounter
	workerTiming    durationCounter
}

func newAudioPipeline(e *Engine) *audioPipeline {
	return &audioPipeline{
		engine:   e,
		capture:  newPCMRing(FrameSizeSamples * Channels * 8),
		playback: newPCMRing(FrameSizeSamples * Channels * 4),
		wake:     make(chan struct{}, 1),
		timing:   e.timingEnabled,
	}
}

// callback never calls the codec, peers, network or user code. The backend
// supplies borrowed byte slices in little-endian interleaved float32 format.
func (p *audioPipeline) callback(output, input []byte, frameCount uint32) {
	if p.timing {
		start := time.Now()
		defer func() { p.callbackTiming.observe(time.Since(start), time.Duration(frameCount)*time.Second/SampleRate) }()
	}
	wanted := int(frameCount) * Channels * 4
	available := min(wanted, len(input)) &^ 3
	written := p.capture.writeBytes(input[:available])
	written += p.capture.writeSilence(wanted - available)
	if written < wanted {
		p.captureDropped.Add(uint64((wanted - written) / 4))
	}

	readable := min(wanted, len(output)) &^ 3
	n := p.playback.readBytes(output[:readable])
	clear(output[n:])
	if n < readable {
		p.playbackSilence.Add(uint64((readable - n) / 4))
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *audioPipeline) start(send func([]byte)) {
	p.capture.reset()
	p.playback.reset()
	p.captureDropped.Store(0)
	p.playbackDropped.Store(0)
	p.playbackSilence.Store(0)
	p.encodeErrors.Store(0)
	p.decodeErrors.Store(0)
	p.callbackTiming.reset()
	p.workerTiming.reset()
	// A previous call may have left a notification in the buffered channel.
	select {
	case <-p.wake:
	default:
	}
	p.quit = make(chan struct{})
	p.wg.Add(1)
	go p.run(send, p.quit)
}

func (p *audioPipeline) stop() {
	if p.quit == nil {
		return
	}
	close(p.quit)
	p.wg.Wait()
	p.quit = nil
}

func (p *audioPipeline) stats() AudioStats {
	return AudioStats{
		// A third observer can see indices from different instants. Clamp the
		// approximate gauge; ring producer/consumer behavior is unchanged.
		CaptureQueued:   min(max(p.capture.len(), 0), len(p.capture.buf)) / 4,
		PlaybackQueued:  min(max(p.playback.len(), 0), len(p.playback.buf)) / 4,
		TimingEnabled:   p.timing,
		CallbackTiming:  p.callbackTiming.snapshot(),
		WorkerTiming:    p.workerTiming.snapshot(),
		EncodeErrors:    p.encodeErrors.Load(),
		DecodeErrors:    p.decodeErrors.Load(),
		CaptureDropped:  p.captureDropped.Load(),
		PlaybackDropped: p.playbackDropped.Load(),
		PlaybackSilence: p.playbackSilence.Load(),
	}
}

func (p *audioPipeline) run(send func([]byte), quit <-chan struct{}) {
	defer p.wg.Done()
	w := newPCMWorker()

	for {
		select {
		case <-quit:
			return
		case <-p.wake:
		}
		for p.capture.len() >= frameBytes {
			select {
			case <-quit:
				return
			default:
			}
			p.capture.readBytes(w.input)
			p.processFrame(w, send)
		}
	}
}

// pcmWorker owns the buffers used by the production worker and its benchmarks.
// processFrame requires one complete capture frame and a single caller.
type pcmWorker struct {
	input, encoded, output []byte
	pcm, decoded, mixed    []float32
}

func newPCMWorker() *pcmWorker {
	return &pcmWorker{
		input:   make([]byte, frameBytes),
		pcm:     make([]float32, FrameSizeSamples*Channels),
		encoded: make([]byte, MaxEncodedSize+2),
		decoded: make([]float32, FrameSizeSamples*Channels),
		mixed:   make([]float32, FrameSizeSamples*Channels),
		output:  make([]byte, frameBytes),
	}
}

func (p *audioPipeline) processFrame(w *pcmWorker, send func([]byte)) {
	if p.timing {
		start := time.Now()
		defer func() { p.workerTiming.observe(time.Since(start), FrameSizeMs*time.Millisecond) }()
	}
	muted := p.engine.IsMuted()
	for i := range w.pcm {
		if muted {
			w.pcm[i] = 0
		} else {
			w.pcm[i] = math.Float32frombits(binary.LittleEndian.Uint32(w.input[i*4:]))
		}
	}
	n, err := p.engine.enc.EncodeFloat32(w.pcm, w.encoded[2:])
	if err != nil {
		p.encodeErrors.Add(1)
	}
	if err == nil && n > 0 && send != nil {
		binary.BigEndian.PutUint16(w.encoded[:2], p.engine.sendSeq)
		p.engine.sendSeq++
		// The callback must copy this borrowed packet before returning.
		send(w.encoded[:n+2])
	}

	clear(w.mixed)
	p.engine.mixMu.RLock()
	for _, jb := range p.engine.inputs.Load().(map[string]*JitterBuffer) {
		packetPtr := jb.Pop()
		var packet []byte
		if packetPtr != nil {
			packet = (*packetPtr)[2:]
		}
		n, err := jb.decode(packet, w.decoded)
		if packetPtr != nil {
			releasePacket(packetPtr)
		}
		if err != nil {
			p.decodeErrors.Add(1)
		}
		if err == nil {
			for i := range min(n*Channels, len(w.mixed)) {
				w.mixed[i] += w.decoded[i]
			}
		}
	}
	p.engine.mixMu.RUnlock()
	for i, sample := range w.mixed {
		if sample > 1 {
			sample = 1
		} else if sample < -1 {
			sample = -1
		}
		binary.LittleEndian.PutUint32(w.output[i*4:], math.Float32bits(sample))
	}
	if p.playback.len()+frameBytes <= len(p.playback.buf) {
		p.playback.writeBytes(w.output)
	} else {
		p.playbackDropped.Add(FrameSizeSamples * Channels)
	}
}
