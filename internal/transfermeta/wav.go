package transfermeta

import (
	"encoding/binary"
	"io"
)

// Only PCM/IEEE float WAVE is supported. Rebuild the container from fmt/data
// chunks and functional cue/loop/sample-count chunks. BWF/iXML and unknown
// variants require explicit original consent; their timing semantics may matter.
// Audio sample bytes are copied without transcoding.
func sanitizeWAV(src io.ReadSeeker, dst io.Writer, size int64) (bool, error) {
	var header [12]byte
	if _, err := io.ReadFull(src, header[:]); err != nil {
		return false, err
	}
	if string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" || int64(binary.LittleEndian.Uint32(header[4:8]))+8 != size {
		return false, ErrMalformed
	}
	type chunk struct {
		offset, size int64
	}
	var chunks []chunk
	var total int64 = 4
	hasFormat, hasData := false, false
	for pos := int64(12); pos < size; {
		var h [8]byte
		if _, err := io.ReadFull(src, h[:]); err != nil {
			return false, err
		}
		n := int64(binary.LittleEndian.Uint32(h[4:]))
		if n > size-pos-8 || n+n%2 > size-pos-8 {
			return false, ErrMalformed
		}
		tag := string(h[:4])
		if tag == "fmt " {
			if hasFormat || (n != 16 && n != 18) {
				return false, ErrUnsupported
			}
			var fmt [18]byte
			if _, err := io.ReadFull(src, fmt[:n]); err != nil {
				return false, err
			}
			code := binary.LittleEndian.Uint16(fmt[:2])
			if code != 1 && code != 3 || n == 18 && binary.LittleEndian.Uint16(fmt[16:]) != 0 {
				return false, ErrUnsupported
			}
			hasFormat = true
		} else if tag == "data" {
			hasData = true
		}
		keep := false
		switch tag {
		case "fmt ", "data", "fact", "cue ", "smpl", "inst", "plst":
			keep = true
		case "LIST":
			if n < 4 {
				return false, ErrUnsupported
			}
			var listType [4]byte
			if _, err := src.Seek(pos+8, io.SeekStart); err != nil {
				return false, err
			}
			if _, err := io.ReadFull(src, listType[:]); err != nil {
				return false, err
			}
			switch string(listType[:]) {
			case "INFO":
			case "adtl":
				keep = true
			default:
				return false, ErrUnsupported
			}
		case "XMP ", "JUNK", "PAD ": // Optional descriptive metadata or padding.
		default:
			return false, ErrUnsupported // BWF/iXML can control synchronisation and track interpretation.
		}
		if keep {
			chunks = append(chunks, chunk{pos, n})
			total += 8 + n + n%2
		}
		pos += 8 + n + n%2
		if _, err := src.Seek(pos, io.SeekStart); err != nil {
			return false, err
		}
	}
	if !hasFormat || !hasData {
		return false, ErrMalformed
	}
	binary.LittleEndian.PutUint32(header[4:8], uint32(total))
	if _, err := dst.Write(header[:]); err != nil {
		return false, err
	}
	for _, c := range chunks {
		if _, err := src.Seek(c.offset, io.SeekStart); err != nil {
			return false, err
		}
		if _, err := io.CopyN(dst, src, 8+c.size); err != nil {
			return false, err
		}
		if c.size%2 != 0 {
			if _, err := dst.Write([]byte{0}); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}
