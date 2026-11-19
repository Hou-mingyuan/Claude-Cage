package transfermeta

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
)

// Walk every scan, including progressive JPEGs. Only stuffed bytes and restart
// markers are opaque; APP/COM markers between scans still pass through policy.
func sanitizeJPEG(src io.ReadSeeker, dst io.Writer) (bool, error) {
	r := bufio.NewReaderSize(src, 64<<10)
	var soi [2]byte
	if _, err := io.ReadFull(r, soi[:]); err != nil {
		return false, err
	}
	if soi != [2]byte{0xff, 0xd8} {
		return false, ErrMalformed
	}
	if _, err := dst.Write(soi[:]); err != nil {
		return false, err
	}
	var pending byte
	for {
		marker := pending
		pending = 0
		if marker == 0 {
			prefix, err := r.ReadByte()
			if err != nil {
				return false, err
			}
			if prefix != 0xff {
				return false, ErrMalformed
			}
			for marker == 0 || marker == 0xff {
				marker, err = r.ReadByte()
				if err != nil {
					return false, err
				}
				if marker == 0 {
					return false, ErrMalformed
				}
			}
		}
		if marker == 0xd9 {
			if _, err := dst.Write([]byte{0xff, marker}); err != nil {
				return false, err
			}
			// Data after EOI is not part of the displayed JPEG.
			return true, nil
		}
		if marker == 0xd8 {
			return false, ErrMalformed
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			if _, err := dst.Write([]byte{0xff, marker}); err != nil {
				return false, err
			}
			continue
		}
		var length [2]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return false, err
		}
		n := int(binary.BigEndian.Uint16(length[:])) - 2
		if n < 0 {
			return false, ErrMalformed
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return false, err
		}
		if jpegMetadataMarker(marker) {
			if marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
				if orientation := exifOrientation(payload[6:]); orientation != 0 {
					if err := writeJPEGSegment(dst, 0xe1, append([]byte("Exif\x00\x00"), orientationEXIF(orientation)...)); err != nil {
						return false, err
					}
				}
			}
			continue
		}
		// APP0/2/14 are kept only for their defined display signatures.
		if marker == 0xe0 && !bytes.HasPrefix(payload, []byte("JFIF\x00")) ||
			marker == 0xe2 && !bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")) ||
			marker == 0xee && !bytes.HasPrefix(payload, []byte("Adobe")) {
			continue
		}
		if err := writeJPEGSegment(dst, marker, payload); err != nil {
			return false, err
		}
		if marker == 0xda {
			var err error
			pending, err = copyJPEGScan(r, dst)
			if err != nil {
				return false, err
			}
		}
	}
}

func writeJPEGSegment(dst io.Writer, marker byte, payload []byte) error {
	n := len(payload) + 2
	if _, err := dst.Write([]byte{0xff, marker, byte(n >> 8), byte(n)}); err != nil {
		return err
	}
	_, err := dst.Write(payload)
	return err
}

func copyJPEGScan(r *bufio.Reader, dst io.Writer) (byte, error) {
	for {
		data, err := r.ReadSlice(0xff)
		if err == bufio.ErrBufferFull {
			if _, e := dst.Write(data); e != nil {
				return 0, e
			}
			continue
		}
		if err != nil {
			return 0, err
		}
		if _, err := dst.Write(data[:len(data)-1]); err != nil {
			return 0, err
		}
		marker, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		for marker == 0xff {
			marker, err = r.ReadByte()
			if err != nil {
				return 0, err
			}
		}
		if marker == 0 || marker >= 0xd0 && marker <= 0xd7 {
			if _, err := dst.Write([]byte{0xff, marker}); err != nil {
				return 0, err
			}
			continue
		}
		return marker, nil
	}
}

func jpegMetadataMarker(marker byte) bool {
	return marker == 0xfe || marker >= 0xe1 && marker <= 0xef && marker != 0xe2 && marker != 0xee
}

// Rebuild a tiny EXIF containing only the display orientation. Never copy an
// IFD pointer, GPS subdirectory, thumbnail, timestamp or other original field.
func exifOrientation(data []byte) uint16 {
	if len(data) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	if order.Uint16(data[2:4]) != 42 {
		return 0
	}
	offset := uint64(order.Uint32(data[4:8]))
	if offset+2 > uint64(len(data)) {
		return 0
	}
	count := uint64(order.Uint16(data[offset : offset+2]))
	if offset+2+count*12+4 > uint64(len(data)) {
		return 0
	}
	for i := uint64(0); i < count; i++ {
		entry := data[offset+2+i*12 : offset+2+(i+1)*12]
		if order.Uint16(entry[:2]) == 0x112 && order.Uint16(entry[2:4]) == 3 && order.Uint32(entry[4:8]) == 1 {
			v := order.Uint16(entry[8:10])
			if v >= 1 && v <= 8 {
				return v
			}
		}
	}
	return 0
}

func orientationEXIF(v uint16) []byte {
	data := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(data[18:20], v)
	return data
}
