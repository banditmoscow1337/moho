package moho

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"

	mp3 "github.com/hajimehoshi/go-mp3"
)

type AudioCodec uint8

const (
	CodecWAV AudioCodec = iota
	CodecMP3
	CodecWAVFloat
)

func OpenAudio(file fs.File) (io.Reader, PCMFormat, int64, AudioCodec, error) {
	reader := bufio.NewReader(file)
	header, err := reader.Peek(4)
	if err != nil {
		return nil, PCMFormat{}, 0, 0, err
	}
	if string(header) == "RIFF" {
		format, length, code, err := readWaveHeader(reader)
		if err != nil {
			return nil, PCMFormat{}, 0, 0, err
		}
		if code == 3 {
			format.BitsPerSample = 16
			return &float32PCM{source: io.LimitReader(reader, length)}, format, length / 2, CodecWAVFloat, nil
		}
		return io.LimitReader(reader, length), format, length, CodecWAV, nil
	}
	if string(header[:3]) == "ID3" || header[0] == 0xff && header[1]&0xe0 == 0xe0 {
		decoder, err := mp3.NewDecoder(reader)
		if err != nil {
			return nil, PCMFormat{}, 0, 0, fmt.Errorf("MP3 decoder: %w", err)
		}
		format := PCMFormat{Channels: 2, SampleRate: decoder.SampleRate(), BitsPerSample: 16}
		if format.SampleRate < 8000 || format.SampleRate > 192000 {
			return nil, PCMFormat{}, 0, 0, fmt.Errorf("unsupported MP3 sample rate %d", format.SampleRate)
		}
		return decoder, format, -1, CodecMP3, nil
	}
	return nil, PCMFormat{}, 0, 0, fmt.Errorf("unsupported audio header %x (expected PCM WAV or MP3)", header)
}

func readWaveHeader(r io.Reader) (PCMFormat, int64, uint16, error) {
	var header [12]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return PCMFormat{}, 0, 0, err
	}
	if string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" {
		return PCMFormat{}, 0, 0, fmt.Errorf("expected RIFF/WAVE")
	}
	var format PCMFormat
	var code uint16
	for i := 0; i < 128; i++ {
		var chunk [8]byte
		if _, err := io.ReadFull(r, chunk[:]); err != nil {
			return PCMFormat{}, 0, 0, err
		}
		length := int64(binary.LittleEndian.Uint32(chunk[4:]))
		switch string(chunk[:4]) {
		case "fmt ":
			if length < 16 || length > 65536 {
				return PCMFormat{}, 0, 0, fmt.Errorf("unsupported WAV fmt length %d", length)
			}
			var data [16]byte
			if _, err := io.ReadFull(r, data[:]); err != nil {
				return PCMFormat{}, 0, 0, err
			}
			code = binary.LittleEndian.Uint16(data[:2])
			format.Channels = int(binary.LittleEndian.Uint16(data[2:4]))
			format.SampleRate = int(binary.LittleEndian.Uint32(data[4:8]))
			format.BitsPerSample = int(binary.LittleEndian.Uint16(data[14:16]))
			align := int(binary.LittleEndian.Uint16(data[12:14]))
			if !((code == 1 && (format.BitsPerSample == 8 || format.BitsPerSample == 16)) ||
				(code == 3 && format.BitsPerSample == 32)) ||
				(format.Channels != 1 && format.Channels != 2) ||
				format.SampleRate < 8000 || format.SampleRate > 192000 ||
				align != format.Channels*format.BitsPerSample/8 {
				return PCMFormat{}, 0, 0, fmt.Errorf("unsupported WAV format (code=%d channels=%d bits=%d rate=%d)",
					code, format.Channels, format.BitsPerSample, format.SampleRate)
			}
			if _, err := io.CopyN(io.Discard, r, length-16+length%2); err != nil {
				return PCMFormat{}, 0, 0, err
			}
		case "data":
			if format.Channels == 0 {
				return PCMFormat{}, 0, 0, fmt.Errorf("WAV data precedes fmt")
			}
			if length == 0 || length%int64(format.Channels*format.BitsPerSample/8) != 0 {
				return PCMFormat{}, 0, 0, fmt.Errorf("WAV data is empty or incomplete")
			}
			return format, length, code, nil
		default:
			if _, err := io.CopyN(io.Discard, r, length+length%2); err != nil {
				return PCMFormat{}, 0, 0, err
			}
		}
	}
	return PCMFormat{}, 0, 0, fmt.Errorf("WAV has too many chunks")
}
