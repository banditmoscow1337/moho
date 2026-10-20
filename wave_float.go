package moho

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

type float32PCM struct {
	source            io.Reader
	raw               [8192]byte
	converted         [4096]byte
	offset, available int
	finished          bool
}

func (f *float32PCM) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if f.offset == f.available {
		if f.finished {
			return 0, io.EOF
		}
		n, err := io.ReadFull(f.source, f.raw[:])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return 0, err
		}
		if n == 0 {
			f.finished = true
			return 0, io.EOF
		}
		if n%4 != 0 {
			return 0, fmt.Errorf("incomplete float32 WAV sample")
		}
		f.available, f.offset = n/2, 0
		for i := 0; i < n; i += 4 {
			value := math.Float32frombits(binary.LittleEndian.Uint32(f.raw[i:]))
			var sample int16
			switch {
			case math.IsNaN(float64(value)):
				sample = 0
			case value <= -1:
				sample = -32768
			case value >= 1:
				sample = 32767
			default:
				sample = int16(value * 32768)
			}
			binary.LittleEndian.PutUint16(f.converted[i/2:], uint16(sample))
		}
		if err != nil {
			f.finished = true
		}
	}
	n := copy(p, f.converted[f.offset:f.available])
	f.offset += n
	return n, nil
}
