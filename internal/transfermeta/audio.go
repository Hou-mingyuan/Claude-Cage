package transfermeta

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"unicode/utf16"
)

func sanitizeFLAC(src io.ReadSeeker, dst io.Writer) (bool, error) {
	var sig [4]byte
	if _, err := io.ReadFull(src, sig[:]); err != nil || string(sig[:]) != "fLaC" {
		return false, ErrUnsupported
	}
	var blocks [][]byte
	changed := false
	total := 0
	for index := 0; index < 4096; index++ {
		var h [4]byte
		if _, err := io.ReadFull(src, h[:]); err != nil {
			return false, ErrUnsupported
		}
		typ := h[0] & 127
		n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
		total += n
		if n > 8<<20 || total > 64<<20 {
			return false, ErrUnsupported
		}
		if index == 0 && (typ != 0 || n != 34) || index > 0 && typ == 0 || typ == 127 {
			return false, ErrUnsupported
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(src, b); err != nil {
			return false, ErrUnsupported
		}
		if typ == 4 {
			clean, c, err := cleanVorbisComments(b)
			if err != nil {
				return false, err
			}
			b = clean
			changed = changed || c
		} else if typ == 6 {
			clean, c, err := cleanFLACPicture(b)
			if err != nil {
				return false, err
			}
			b = clean
			changed = changed || c
		} else if typ == 2 || typ > 6 {
			return false, ErrUnsupported
		} // Unknown application blocks may be functional.
		block := append([]byte{typ, byte(len(b) >> 16), byte(len(b) >> 8), byte(len(b))}, b...)
		blocks = append(blocks, block)
		if h[0]&128 != 0 {
			break
		}
		if index == 4095 {
			return false, ErrUnsupported
		}
	}
	if _, err := dst.Write(sig[:]); err != nil {
		return false, err
	}
	for i, b := range blocks {
		if i == len(blocks)-1 {
			b[0] |= 128
		}
		if _, err := dst.Write(b); err != nil {
			return false, err
		}
	}
	_, err := io.Copy(dst, src)
	return changed, err
}

func cleanFLACPicture(b []byte) ([]byte, bool, error) {
	if len(b) < 8 {
		return nil, false, ErrUnsupported
	}
	pos := 4
	readString := func() ([]byte, error) {
		if len(b)-pos < 4 {
			return nil, ErrUnsupported
		}
		n := uint64(binary.BigEndian.Uint32(b[pos:]))
		pos += 4
		if n > uint64(len(b)-pos) {
			return nil, ErrUnsupported
		}
		s := b[pos : pos+int(n)]
		pos += int(n)
		return s, nil
	}
	mime, err := readString()
	if err != nil {
		return nil, false, err
	}
	if _, err := readString(); err != nil {
		return nil, false, err
	}
	if len(b)-pos < 20 {
		return nil, false, ErrUnsupported
	}
	pos += 16
	n := uint64(binary.BigEndian.Uint32(b[pos:]))
	pos += 4
	if n != uint64(len(b)-pos) {
		return nil, false, ErrUnsupported
	}
	if string(mime) == "-->" {
		return b, false, nil
	} // Linked artwork is content, never fetched.
	ext := ""
	switch string(mime) {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	default:
		return nil, false, ErrUnsupported
	}
	clean, err := SanitizeBytes("cover"+ext, b[pos:])
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(clean, b[pos:]) {
		return b, false, nil
	}
	if len(clean)+pos > 0xffffff {
		return nil, false, ErrUnsupported
	}
	out := bytes.Clone(b[:pos])
	binary.BigEndian.PutUint32(out[pos-4:], uint32(len(clean)))
	out = append(out, clean...)
	return out, true, nil
}

func cleanVorbisComments(b []byte) ([]byte, bool, error) {
	pos := 0
	read := func() ([]byte, error) {
		if len(b)-pos < 4 {
			return nil, ErrUnsupported
		}
		n := uint64(binary.LittleEndian.Uint32(b[pos:]))
		pos += 4
		if n > uint64(len(b)-pos) {
			return nil, ErrUnsupported
		}
		v := b[pos : pos+int(n)]
		pos += int(n)
		return v, nil
	}
	vendor, err := read()
	if err != nil {
		return nil, false, err
	}
	if len(b)-pos < 4 {
		return nil, false, ErrUnsupported
	}
	count := binary.LittleEndian.Uint32(b[pos:])
	pos += 4
	if count > uint32(len(b)/4) {
		return nil, false, ErrUnsupported
	}
	var entries [][]byte
	changed := false
	for i := uint32(0); i < count; i++ {
		v, err := read()
		if err != nil {
			return nil, false, err
		}
		key, value, ok := strings.Cut(string(v), "=")
		if ok && (regionalKey(key) || regionalDate(key, value)) {
			changed = true
			continue
		}
		entries = append(entries, v)
	}
	if pos != len(b) {
		return nil, false, ErrUnsupported
	}
	var out bytes.Buffer
	write := func(v []byte) { _ = binary.Write(&out, binary.LittleEndian, uint32(len(v))); out.Write(v) }
	write(vendor)
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(entries)))
	for _, v := range entries {
		write(v)
	}
	return out.Bytes(), changed, nil
}

func syncSafe(b []byte) (int, error) {
	if len(b) != 4 {
		return 0, ErrUnsupported
	}
	n := 0
	for _, v := range b {
		if v&128 != 0 {
			return 0, ErrUnsupported
		}
		n = n<<7 | int(v)
	}
	return n, nil
}

func id3Text(b []byte) (string, error) {
	if len(b) == 0 {
		return "", ErrUnsupported
	}
	enc := b[0]
	b = b[1:]
	if enc == 0 || enc == 3 {
		return string(b), nil
	}
	var order binary.ByteOrder = binary.BigEndian
	if enc == 1 {
		if len(b) < 2 {
			return "", ErrUnsupported
		}
		switch {
		case b[0] == 255 && b[1] == 254:
			order = binary.LittleEndian
		case b[0] == 254 && b[1] == 255:
		default:
			return "", ErrUnsupported
		}
		b = b[2:]
	} else if enc != 2 {
		return "", ErrUnsupported
	}
	if len(b)%2 != 0 {
		return "", ErrUnsupported
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = order.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u)), nil
}

func sanitizeMP3(src io.ReadSeeker, dst io.Writer, size int64) (bool, error) {
	var h [10]byte
	if _, err := io.ReadFull(src, h[:]); err != nil {
		return false, ErrUnsupported
	}
	if string(h[:3]) != "ID3" {
		return false, ErrUnsupported
	}
	version := h[3]
	if version != 3 && version != 4 || h[5]&^byte(0x10) != 0 || version == 3 && h[5] != 0 {
		return false, ErrUnsupported
	}
	n, err := syncSafe(h[6:])
	if err != nil || n > 16<<20 || int64(n)+10 > size {
		return false, ErrUnsupported
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(src, b); err != nil {
		return false, ErrUnsupported
	}
	var kept []byte
	changed := false
	for pos := 0; pos < len(b); {
		if b[pos] == 0 {
			for _, v := range b[pos:] {
				if v != 0 {
					return false, ErrUnsupported
				}
			}
			break
		}
		if len(b)-pos < 10 {
			return false, ErrUnsupported
		}
		frame := b[pos : pos+10]
		id := string(frame[:4])
		length := int(binary.BigEndian.Uint32(frame[4:8]))
		if version == 4 {
			length, err = syncSafe(frame[4:8])
			if err != nil {
				return false, err
			}
		}
		if length < 0 || length > len(b)-pos-10 || frame[9] != 0 {
			return false, ErrUnsupported
		}
		payload := b[pos+10 : pos+10+length]
		drop := false
		if id == "TXXX" {
			text, err := id3Text(payload)
			if err != nil {
				return false, err
			}
			key, value, _ := strings.Cut(text, "\x00")
			drop = regionalKey(key) || regionalDate(key, value)
		} else if id == "PRIV" {
			owner, _, _ := strings.Cut(string(payload), "\x00")
			drop = regionalKey(owner)
		} else if id == "TDRC" || id == "TDRL" || id == "TDEN" || id == "TDTG" {
			text, err := id3Text(payload)
			if err != nil {
				return false, err
			}
			drop = regionalDate("date", text)
		}
		if drop {
			changed = true
		} else {
			kept = append(kept, b[pos:pos+10+length]...)
		}
		pos += 10 + length
	}
	if version == 4 && h[5]&16 != 0 {
		return false, ErrUnsupported
	} // Footer variants use explicit original for now.
	// Do not silently claim to clean trailing APE/private containers.
	if size >= 32 {
		if _, err := src.Seek(size-32, io.SeekStart); err != nil {
			return false, err
		}
		var tail [32]byte
		if _, err := io.ReadFull(src, tail[:]); err != nil {
			return false, err
		}
		if bytes.Contains(tail[:], []byte("APETAGEX")) {
			return false, ErrUnsupported
		}
	}
	if _, err := src.Seek(int64(n)+10, io.SeekStart); err != nil {
		return false, err
	}
	var sync [2]byte
	if _, err := io.ReadFull(src, sync[:]); err != nil || sync[0] != 255 || sync[1]&224 != 224 {
		return false, ErrUnsupported
	}
	if _, err := src.Seek(int64(n)+10, io.SeekStart); err != nil {
		return false, err
	}
	if _, err := dst.Write(h[:]); err != nil {
		return false, err
	}
	out := make([]byte, n)
	copy(out, kept)
	if _, err := dst.Write(out); err != nil {
		return false, err
	}
	_, err = io.Copy(dst, src)
	return changed, err
}
