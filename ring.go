package audio

import "sync/atomic"

// pcmRing has one writer and one reader. Both indices count bytes and only
// advance after their side has finished copying. Capacity is a multiple of
// one float32 sample, so the reader never sees half of a sample.
type pcmRing struct {
	buf   []byte
	read  atomic.Uint64
	write atomic.Uint64
}

func newPCMRing(samples int) pcmRing {
	return pcmRing{buf: make([]byte, samples*4)}
}

func (r *pcmRing) len() int {
	return int(r.write.Load() - r.read.Load())
}

func (r *pcmRing) writeBytes(src []byte) int {
	read := r.read.Load()
	write := r.write.Load()
	n := min(len(src)&^3, len(r.buf)-int(write-read))
	n &^= 3
	if n == 0 {
		return 0
	}
	offset := int(write % uint64(len(r.buf)))
	first := copy(r.buf[offset:], src[:n])
	copy(r.buf, src[first:n])
	r.write.Store(write + uint64(n))
	return n
}

func (r *pcmRing) writeSilence(bytes int) int {
	read := r.read.Load()
	write := r.write.Load()
	n := min(bytes&^3, len(r.buf)-int(write-read))
	n &^= 3
	if n == 0 {
		return 0
	}
	offset := int(write % uint64(len(r.buf)))
	first := min(n, len(r.buf)-offset)
	clear(r.buf[offset : offset+first])
	clear(r.buf[:n-first])
	r.write.Store(write + uint64(n))
	return n
}

func (r *pcmRing) readBytes(dst []byte) int {
	read := r.read.Load()
	write := r.write.Load()
	n := min(len(dst)&^3, int(write-read))
	n &^= 3
	if n == 0 {
		return 0
	}
	offset := int(read % uint64(len(r.buf)))
	first := copy(dst[:n], r.buf[offset:])
	copy(dst[first:n], r.buf[:n-first])
	r.read.Store(read + uint64(n))
	return n
}

// reset requires the device callback and worker to have both stopped.
func (r *pcmRing) reset() {
	r.read.Store(0)
	r.write.Store(0)
}
