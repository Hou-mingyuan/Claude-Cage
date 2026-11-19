package transfermeta

import (
	"encoding/binary"
	"io"
)

// WebP stores EXIF and XMP as top-level RIFF chunks. They are removed along
// with their padding; the RIFF size field is recomputed so the container stays
// valid. Image and animation chunks are copied verbatim.
func sanitizeWebP(src io.ReadSeeker, dst io.Writer, size int64) (bool, error) {
	removed, riffSize, changed, err := scanWebP(src, size)
	if err != nil {
		return false, err
	}
	if !changed {
		if _, err := src.Seek(0, 0); err != nil {
			return false, err
		}
		if _, err := io.Copy(dst, src); err != nil {
			return false, err
		}
		return false, nil
	}
	if _, err := src.Seek(0, 0); err != nil {
		return false, err
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(src, header); err != nil {
		return false, ErrMalformed
	}
	newSize := riffSize - removed
	if newSize < 4 {
		return false, ErrMalformed
	}
	outHeader := make([]byte, 12)
	copy(outHeader[:4], "RIFF")
	binary.LittleEndian.PutUint32(outHeader[4:8], uint32(newSize))
	copy(outHeader[8:12], "WEBP")
	if _, err := dst.Write(outHeader); err != nil {
		return false, err
	}
	position := int64(12)
	end := 8 + riffSize
	for position < end {
		var chunk [8]byte
		if _, err := io.ReadFull(src, chunk[:]); err != nil {
			return false, ErrMalformed
		}
		length := int64(binary.LittleEndian.Uint32(chunk[4:8]))
		padding := length % 2
		total := 8 + length + padding
		if position+total > end {
			return false, ErrMalformed
		}
		if webPMetadataChunk(string(chunk[:4])) {
			if _, err := io.CopyN(io.Discard, src, length+padding); err != nil {
				return false, err
			}
			position += total
			continue
		}
		if _, err := dst.Write(chunk[:]); err != nil {
			return false, err
		}
		if string(chunk[:4]) == "VP8X" && length == 10 {
			// Clear the EXIF and XMP bits in the extended header so the
			// container does not advertise chunks that no longer exist.
			payload := make([]byte, length)
			if _, err := io.ReadFull(src, payload); err != nil {
				return false, ErrMalformed
			}
			payload[0] &^= (0x08 | 0x04)
			if _, err := dst.Write(payload); err != nil {
				return false, err
			}
			if padding > 0 {
				var pad [1]byte
				if _, err := io.ReadFull(src, pad[:]); err != nil {
					return false, ErrMalformed
				}
				if _, err := dst.Write(pad[:]); err != nil {
					return false, err
				}
			}
			position += total
			continue
		}
		if _, err := io.CopyN(dst, src, length+padding); err != nil {
			return false, err
		}
		position += total
	}
	return true, nil
}

func scanWebP(src io.ReadSeeker, size int64) (removed int64, riffSize int64, changed bool, err error) {
	if size < 12 {
		return 0, 0, false, ErrMalformed
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(src, header); err != nil {
		return 0, 0, false, err
	}
	if string(header[:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return 0, 0, false, ErrMalformed
	}
	riffSize = int64(binary.LittleEndian.Uint32(header[4:8]))
	end := 8 + riffSize
	if end != size || riffSize < 4 {
		return 0, 0, false, ErrMalformed
	}
	position := int64(12)
	for position < end {
		var chunk [8]byte
		if _, err := io.ReadFull(src, chunk[:]); err != nil {
			return 0, 0, false, ErrMalformed
		}
		length := int64(binary.LittleEndian.Uint32(chunk[4:8]))
		padding := length % 2
		total := 8 + length + padding
		if position+total > end {
			return 0, 0, false, ErrMalformed
		}
		if webPMetadataChunk(string(chunk[:4])) {
			removed += total
			changed = true
		}
		if _, err := io.CopyN(io.Discard, src, length+padding); err != nil {
			return 0, 0, false, err
		}
		position += total
	}
	return removed, riffSize, changed, nil
}

func webPMetadataChunk(chunkType string) bool {
	switch chunkType {
	case "VP8X", "ICCP", "ALPH", "VP8 ", "VP8L", "ANIM", "ANMF":
		return false
	}
	return true
}
