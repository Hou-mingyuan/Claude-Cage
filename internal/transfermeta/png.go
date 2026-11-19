package transfermeta

import (
	"bytes"
	"encoding/binary"
	"io"
)

// PNG text, EXIF and modification-time chunks are provenance metadata. Colour
// chunks (iCCP, sRGB, gAMA, cHRM), physical-size chunks and image data are
// retained so pixels and rendering do not change.
func sanitizePNG(src io.ReadSeeker, dst io.Writer) (bool, error) {
	signature := make([]byte, len(pngSignature))
	if _, err := io.ReadFull(src, signature); err != nil {
		return false, ErrMalformed
	}
	if !bytes.Equal(signature, pngSignature) {
		return false, ErrMalformed
	}
	if _, err := dst.Write(signature); err != nil {
		return false, err
	}
	for {
		var header [8]byte
		if _, err := io.ReadFull(src, header[:]); err != nil {
			return false, ErrMalformed
		}
		length := binary.BigEndian.Uint32(header[:4])
		chunkType := string(header[4:8])
		if chunkType == "IEND" && length != 0 || chunkType == "IHDR" && length != 13 {
			return false, ErrMalformed
		}
		if int64(length) > MaxBytes {
			return false, ErrMalformed
		}
		if pngMetadataChunk(chunkType) {
			// Length, payload and CRC are all removed.
			if _, err := io.CopyN(io.Discard, src, int64(length)+4); err != nil {
				return false, err
			}
		} else {
			if _, err := dst.Write(header[:]); err != nil {
				return false, err
			}
			if _, err := io.CopyN(dst, src, int64(length)); err != nil {
				return false, err
			}
			var crc [4]byte
			if _, err := io.ReadFull(src, crc[:]); err != nil {
				return false, ErrMalformed
			}
			if _, err := dst.Write(crc[:]); err != nil {
				return false, err
			}
		}
		if chunkType == "IEND" {
			return true, nil
		}
	}
}

func pngMetadataChunk(chunkType string) bool {
	switch chunkType {
	case "IHDR", "PLTE", "IDAT", "IEND", "cHRM", "gAMA", "iCCP", "sRGB", "pHYs", "sBIT", "bKGD", "tRNS", "hIST", "acTL", "fcTL", "fdAT", "cICP", "mDCV", "cLLI":
		return false
	}
	return true
}
