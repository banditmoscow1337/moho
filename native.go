//go:build !safem_test_noaudio

package audio

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// pcmStream owns one direction of a native audio connection. Each direction is
// used by exactly one goroutine until it is closed after that goroutine exits.
type pcmStream interface {
	transfer([]byte) (int, error)
	close() error
}

type nativeBackend interface {
	Backend
	openStreams(DeviceConfig) (capture, playback pcmStream, err error)
}

// nativeDevice bridges the independent capture and playback clocks. A bounded
// queue keeps the latest microphone data; the output clock drives the callback.
// This also keeps native I/O and encoding on different goroutines.
type nativeDevice struct {
	backend            nativeBackend
	config             DeviceConfig
	callback           DataCallback
	mu                 sync.Mutex
	stop               chan struct{}
	done               chan struct{}
	closed             bool
	reconnects         atomic.Uint64
	openFailures       atomic.Uint64
	lastReconnectError atomic.Value // string
}

func (d *nativeDevice) reconnectStats() (uint64, uint64, string) {
	var reason string
	if v := d.lastReconnectError.Load(); v != nil {
		reason = v.(string)
	}
	return d.reconnects.Load(), d.openFailures.Load(), reason
}

func openNativeDevice(b nativeBackend, c DeviceConfig, cb DataCallback) (Device, error) {
	if cb == nil || c.SampleRate == 0 || c.Channels == 0 || c.PeriodMillis == 0 ||
		uint64(c.SampleRate)*uint64(c.PeriodMillis)/1000 == 0 {
		return nil, errors.New("invalid native audio configuration")
	}
	return &nativeDevice{backend: b, config: c, callback: cb}, nil
}

func (d *nativeDevice) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("audio device closed")
	}
	if d.done != nil {
		return errors.New("audio device already started")
	}
	capture, playback, err := d.backend.openStreams(d.config)
	if err != nil {
		return err
	}
	d.stop, d.done = make(chan struct{}), make(chan struct{})
	go d.run(d.stop, d.done, capture, playback)
	return nil
}

func (d *nativeDevice) Stop() error {
	d.mu.Lock()
	done := d.done
	if done != nil {
		close(d.stop)
		d.done = nil
	}
	d.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}

func (d *nativeDevice) Close() error {
	_ = d.Stop()
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

type captureQueue struct {
	mu   sync.Mutex
	data []byte
	used int
}

func (q *captureQueue) push(src []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(src) > len(q.data) {
		src = src[len(src)-len(q.data):]
	}
	if excess := q.used + len(src) - len(q.data); excess > 0 {
		copy(q.data, q.data[excess:q.used])
		q.used -= excess
	}
	q.used += copy(q.data[q.used:], src)
}

func (q *captureQueue) pop(dst []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	// When capture runs ahead, discard the oldest frames to bound latency.
	if q.used > len(dst)*2 {
		excess := q.used - len(dst)*2
		copy(q.data, q.data[excess:q.used])
		q.used -= excess
	}
	n := copy(dst, q.data[:q.used])
	copy(q.data, q.data[n:q.used])
	q.used -= n
	clear(dst[n:])
}

func (d *nativeDevice) run(stop <-chan struct{}, done chan<- struct{}, capture, playback pcmStream) {
	defer close(done)
	for {
		reason := d.pump(stop, capture, playback)
		_ = playback.close()
		if capture != nil {
			_ = capture.close()
		}
		select {
		case <-stop:
			return
		default:
		}
		d.reconnects.Add(1)
		if reason != nil {
			d.lastReconnectError.Store(reason.Error())
		}
		// Transient unplug or server failure: reopen the selected endpoints,
		// including system defaults which may now point to different devices.
		for {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
			}
			var err error
			capture, playback, err = d.backend.openStreams(d.config)
			if err == nil {
				break
			}
			d.openFailures.Add(1)
		}
	}
}

func (d *nativeDevice) pump(stop <-chan struct{}, capture, playback pcmStream) error {
	frames := int(uint64(d.config.SampleRate) * uint64(d.config.PeriodMillis) / 1000)
	size := frames * int(d.config.Channels) * 4
	q := &captureQueue{data: make([]byte, size*4)}
	input, output := make([]byte, size), make([]byte, size)
	period := time.Duration(d.config.PeriodMillis) * time.Millisecond
	readDone := make(chan struct{})
	abort := make(chan struct{})
	if capture != nil {
		go func() {
			defer close(readDone)
			buf := make([]byte, size)
			next := time.Now()
			for {
				select {
				case <-stop:
					return
				case <-abort:
					return
				default:
				}
				n, err := capture.transfer(buf)
				if err != nil {
					return
				}
				if n > 0 {
					q.push(buf[:n])
					// A null/virtual endpoint may not pace reads at all.
					next = next.Add(time.Duration(n) * period / time.Duration(size))
					if delay := time.Until(next); delay > 0 {
						time.Sleep(delay)
					} else {
						next = time.Now()
					}
					continue
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		defer func() { close(abort); <-readDone }()
	}
	// Playback-only devices need a few periods queued before regular pacing.
	// Otherwise each write arrives just as the previous period is consumed,
	// and a small scheduling delay empties the native buffer. Keep the clock
	// for virtual devices that accept writes without applying backpressure.
	ahead := time.Duration(0)
	if d.config.PlaybackOnly {
		ahead = 2 * period
	}
	next := time.Now().Add(-ahead)
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		if capture != nil {
			select {
			case <-readDone:
				return errors.New("capture stopped")
			default:
			}
			q.pop(input)
		} else {
			input = nil
		}
		clear(output)
		d.callback(output, input, uint32(frames))
		for remaining := output; len(remaining) > 0; {
			select {
			case <-stop:
				return nil
			default:
			}
			n, err := playback.transfer(remaining)
			if err != nil {
				return fmt.Errorf("playback transfer: %w", err)
			}
			if n < 0 || n > len(remaining) {
				return errors.New("playback transfer returned invalid byte count")
			}
			remaining = remaining[n:]
			if n == 0 {
				time.Sleep(2 * time.Millisecond)
			}
		}
		next = next.Add(period)
		if delay := time.Until(next); delay > 0 {
			time.Sleep(delay)
		} else if d.config.PlaybackOnly && time.Since(next) > 3*period {
			// Rebuild the safety margin after a long scheduling pause.
			next = time.Now().Add(-ahead)
		} else {
			if !d.config.PlaybackOnly {
				next = time.Now()
			}
		}
	}
}
