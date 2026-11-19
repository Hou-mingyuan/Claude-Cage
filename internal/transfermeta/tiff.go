package transfermeta

import (
	"encoding/binary"
	"io"
)

type fileSpan struct{ start, end int64 }
type tiffCleaner struct {
	r                io.ReaderAt
	size             int64
	order            binary.ByteOrder
	seen             map[int64]bool
	patches          []filePatch
	wipes, protected []fileSpan
}

func sanitizeTIFF(src io.ReadSeeker, dst io.Writer, size int64) (bool, error) {
	r, ok := src.(io.ReaderAt)
	if !ok {
		return false, ErrUnsupported
	}
	t := tiffCleaner{r: r, size: size, seen: map[int64]bool{}}
	h, err := t.read(0, 8)
	if err != nil {
		return false, err
	}
	switch string(h[:2]) {
	case "II":
		t.order = binary.LittleEndian
	case "MM":
		t.order = binary.BigEndian
	default:
		return false, ErrUnsupported
	}
	if t.order.Uint16(h[2:]) != 42 {
		return false, ErrUnsupported
	} // BigTIFF needs its own layout.
	t.protected = append(t.protected, fileSpan{0, 8})
	if t.order.Uint32(h[4:]) == 0 {
		return false, ErrUnsupported
	}
	if err := t.ifd(int64(t.order.Uint32(h[4:])), false, 0); err != nil {
		return false, err
	}
	for _, wipe := range t.wipes {
		for _, keep := range t.protected {
			if wipe.start < keep.end && keep.start < wipe.end {
				return false, ErrUnsupported
			}
		}
	}
	return writePatched(src, dst, t.patches)
}

func (t *tiffCleaner) read(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || n > 8<<20 || off > t.size-n {
		return nil, ErrUnsupported
	}
	b := make([]byte, int(n))
	_, err := t.r.ReadAt(b, off)
	if err != nil {
		return nil, ErrUnsupported
	}
	return b, nil
}

func (t *tiffCleaner) erase(off, n int64) error {
	if _, err := t.read(off, n); err != nil {
		return err
	}
	t.wipes = append(t.wipes, fileSpan{off, off + n})
	t.patches = append(t.patches, filePatch{off, make([]byte, int(n))})
	return nil
}

func (t *tiffCleaner) entryData(e []byte, entryOffset int64) ([]byte, int64, error) {
	typ := int(t.order.Uint16(e[2:]))
	sizes := []int64{0, 1, 1, 2, 4, 8, 1, 1, 2, 4, 8, 4, 8, 4}
	if typ <= 0 || typ >= len(sizes) {
		return nil, 0, ErrUnsupported
	}
	n := int64(t.order.Uint32(e[4:])) * sizes[typ]
	if n <= 4 {
		return e[8 : 8+n], entryOffset + 8, nil
	}
	off := int64(t.order.Uint32(e[8:]))
	b, err := t.read(off, n)
	return b, off, err
}

func (t *tiffCleaner) integers(e, data []byte) ([]int64, error) {
	typ := t.order.Uint16(e[2:])
	var out []int64
	switch typ {
	case 3:
		for i := 0; i < len(data); i += 2 {
			out = append(out, int64(t.order.Uint16(data[i:])))
		}
	case 4, 13:
		for i := 0; i < len(data); i += 4 {
			out = append(out, int64(t.order.Uint32(data[i:])))
		}
	default:
		return nil, ErrUnsupported
	}
	return out, nil
}

func (t *tiffCleaner) ifd(off int64, gps bool, depth int) error {
	if off == 0 {
		return nil
	}
	if depth > 32 || len(t.seen) > 4096 || t.seen[off] {
		return ErrUnsupported
	}
	t.seen[off] = true
	b, err := t.read(off, 2)
	if err != nil {
		return err
	}
	count := int(t.order.Uint16(b))
	dir, err := t.read(off, int64(2+12*count+4))
	if err != nil {
		return err
	}
	next := int64(t.order.Uint32(dir[len(dir)-4:]))
	var kept []byte
	values := map[uint16][]int64{}
	for i := 0; i < count; i++ {
		e := dir[2+i*12 : 2+(i+1)*12]
		tag := t.order.Uint16(e)
		data, dataOff, err := t.entryData(e, off+int64(2+i*12))
		if err != nil {
			return err
		}
		if !gps && (tag == 34735 || tag == 34736 || tag == 34737) {
			return ErrUnsupported
		} // GeoTIFF georeferencing is functional.
		drop := gps || tag == 0x8825 || tag >= 0x9010 && tag <= 0x9012 || tag == 0x882a
		if tag == 0x8825 && !gps {
			ptrs, err := t.integers(e, data)
			if err != nil {
				return err
			}
			for _, p := range ptrs {
				if err := t.ifd(p, true, depth+1); err != nil {
					return err
				}
			}
		}
		if !drop && (tag == 0x8769 || tag == 0xa005 || tag == 330) {
			ptrs, err := t.integers(e, data)
			if err != nil {
				return err
			}
			for _, p := range ptrs {
				if err := t.ifd(p, false, depth+1); err != nil {
					return err
				}
			}
		}
		if drop {
			if len(data) > 4 {
				if err := t.erase(dataOff, int64(len(data))); err != nil {
					return err
				}
			}
			continue
		}
		if tag == 700 { // XMP: remove geographic fields, preserve language/ICC/display settings.
			clean, changed, err := scrubRegionalXML(data)
			if err != nil {
				return err
			}
			if changed {
				t.patches = append(t.patches, filePatch{dataOff, clean})
				t.wipes = append(t.wipes, fileSpan{dataOff, dataOff + int64(len(data))})
			} else if len(data) > 4 {
				t.protected = append(t.protected, fileSpan{dataOff, dataOff + int64(len(data))})
			}
		} else if len(data) > 4 {
			t.protected = append(t.protected, fileSpan{dataOff, dataOff + int64(len(data))})
		}
		switch tag {
		case 273, 279, 324, 325, 513, 514:
			v, err := t.integers(e, data)
			if err != nil {
				return err
			}
			values[tag] = v
		}
		kept = append(kept, e...)
	}
	if gps {
		if err := t.erase(off, int64(len(dir))); err != nil {
			return err
		}
	} else {
		t.protected = append(t.protected, fileSpan{off, off + int64(len(dir))})
		if len(kept) != 12*count {
			out := make([]byte, len(dir))
			t.order.PutUint16(out, uint16(len(kept)/12))
			copy(out[2:], kept)
			t.order.PutUint32(out[2+len(kept):], uint32(next))
			t.patches = append(t.patches, filePatch{off, out})
		}
		for _, pair := range [][2]uint16{{273, 279}, {324, 325}, {513, 514}} {
			offsets, counts := values[pair[0]], values[pair[1]]
			if len(offsets) != len(counts) {
				return ErrUnsupported
			}
			for i, p := range offsets {
				if p < 0 || counts[i] < 0 || p > t.size-counts[i] {
					return ErrUnsupported
				}
				t.protected = append(t.protected, fileSpan{p, p + counts[i]})
			}
		}
	}
	return t.ifd(next, gps, depth+1)
}
