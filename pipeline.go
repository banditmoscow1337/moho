package audio

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var pipelinePool = sync.Pool{
	New: func() any {
		return &audioPipeline{
			capture:  newPCMRing(FrameSizeSamples * Channels * 8),
			playback: newPCMRing(FrameSizeSamples * Channels * 4),
			wake:     make(chan struct{}, 1),
			worker:   newPCMWorker(),
		}
	},
}

const frameBytes = FrameSizeSamples * Channels * 4

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

type audioPipeline struct {
	engine   *Engine
	capture  pcmRing // callback writes, worker reads
	playback pcmRing // worker writes, callback reads
	wake     chan struct{}
	stopping atomic.Bool
	running  bool
	wg       sync.WaitGroup
	worker   *pcmWorker

	captureDropped  atomic.Uint64
	playbackDropped atomic.Uint64
	playbackSilence atomic.Uint64
	encodeErrors    atomic.Uint64
	decodeErrors    atomic.Uint64

	timing         bool
	callbackTiming durationCounter
	workerTiming   durationCounter
}

func newAudioPipeline(e *Engine) *audioPipeline {
	p := pipelinePool.Get().(*audioPipeline)
	p.engine = e
	p.timing = e.timingEnabled
	p.stopping.Store(false)
	p.running = false
	p.capture.reset()
	p.playback.reset()
	p.captureDropped.Store(0)
	p.playbackDropped.Store(0)
	p.playbackSilence.Store(0)
	p.encodeErrors.Store(0)
	p.decodeErrors.Store(0)
	p.callbackTiming.reset()
	p.workerTiming.reset()
	return p
}

func releaseAudioPipeline(p *audioPipeline) {
	if p == nil {
		return
	}
	p.engine = nil
	pipelinePool.Put(p)
}

func (p *audioPipeline) callback(output, input []byte, frameCount uint32) {
	if p.timing {
		start := time.Now()
		defer func() {
			p.callbackTiming.observe(time.Since(start), time.Duration(frameCount)*time.Second/SampleRate)
		}()
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

	select {
	case <-p.wake:
	default:
	}

	p.stopping.Store(false)
	p.running = true
	p.wg.Add(1)
	go p.run(send)
}

func (p *audioPipeline) stop() {
	if !p.running {
		return
	}
	p.stopping.Store(true)
	select {
	case p.wake <- struct{}{}:
	default:
	}
	p.wg.Wait()
	p.running = false
}

func (p *audioPipeline) stats() AudioStats {
	return AudioStats{
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

func (p *audioPipeline) run(send func([]byte)) {
	defer p.wg.Done()
	w := p.worker
	for {
		<-p.wake
		if p.stopping.Load() {
			return
		}
		for p.capture.len() >= frameBytes {
			if p.stopping.Load() {
				return
			}
			p.capture.readBytes(w.input)
			p.processFrame(w, send)
		}
	}
}

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
		defer func() {
			p.workerTiming.observe(time.Since(start), FrameSizeMs*time.Millisecond)
		}()
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