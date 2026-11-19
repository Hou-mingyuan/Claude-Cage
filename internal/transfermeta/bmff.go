package transfermeta

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
)

type mediaBox struct {
	start, payload, end int64
	kind                string
}
type bmffCleaner struct {
	r         io.ReaderAt
	size      int64
	patches   []filePatch
	count     int
	dataSpans []fileSpan
}

func (b *bmffCleaner) read(off, n int64) ([]byte, error) {
	if n < 0 || n > 8<<20 || off < 0 || off > b.size-n {
		return nil, ErrUnsupported
	}
	v := make([]byte, int(n))
	if _, err := b.r.ReadAt(v, off); err != nil {
		return nil, ErrUnsupported
	}
	return v, nil
}

func (b *bmffCleaner) boxes(start, end int64) ([]mediaBox, error) {
	if start < 0 || end < start || end > b.size {
		return nil, ErrUnsupported
	}
	var out []mediaBox
	for start < end {
		b.count++
		if b.count > 65536 || end-start < 8 {
			return nil, ErrUnsupported
		}
		h, err := b.read(start, 8)
		if err != nil {
			return nil, err
		}
		n := int64(binary.BigEndian.Uint32(h))
		header := int64(8)
		if n == 1 {
			h, err = b.read(start+8, 8)
			if err != nil {
				return nil, err
			}
			u := binary.BigEndian.Uint64(h)
			if u > uint64(end-start) {
				return nil, ErrUnsupported
			}
			n = int64(u)
			header = 16
		}
		if n == 0 {
			n = end - start
		}
		if n < header || n > end-start {
			return nil, ErrUnsupported
		}
		tag, err := b.read(start+4, 4)
		if err != nil {
			return nil, err
		}
		out = append(out, mediaBox{start, start + header, start + n, string(tag)})
		start += n
	}
	return out, nil
}

func (b *bmffCleaner) free(box mediaBox) error {
	if box.end-box.payload > 8<<20 {
		return ErrUnsupported
	}
	// Preserve both 32-bit and extended sizes: chunk offsets never move.
	b.patches = append(b.patches, filePatch{box.start + 4, []byte("free")}, filePatch{box.payload, make([]byte, int(box.end-box.payload))})
	return nil
}

func sanitizeBMFF(src io.ReadSeeker, dst io.Writer, size int64) (bool, error) {
	r, ok := src.(io.ReaderAt)
	if !ok {
		return false, ErrUnsupported
	}
	b := bmffCleaner{r: r, size: size}
	top, err := b.boxes(0, size)
	if err != nil {
		return false, err
	}
	for _, box := range top {
		if box.kind == "mdat" {
			b.dataSpans = append(b.dataSpans, fileSpan{box.payload, box.end})
		}
	}
	if err := b.walk(0, size, 0); err != nil {
		return false, err
	}
	return writePatched(src, dst, b.patches)
}

func (b *bmffCleaner) walk(start, end int64, depth int) error {
	if depth > 24 {
		return ErrUnsupported
	}
	boxes, err := b.boxes(start, end)
	if err != nil {
		return err
	}
	for _, box := range boxes {
		switch box.kind {
		case "moov", "trak", "mdia", "minf", "stbl", "edts", "udta", "moof", "traf", "mfra":
			if err := b.walk(box.payload, box.end, depth+1); err != nil {
				return err
			}
		case "meta":
			if err := b.meta(box, depth+1); err != nil {
				return err
			}
		case "\xa9xyz", "loci", "gps ", "gdat", "gdir", "gpri":
			if err := b.free(box); err != nil {
				return err
			}
		case "XMP_":
			v, err := b.read(box.payload, box.end-box.payload)
			if err != nil {
				return err
			}
			clean, changed, err := scrubRegionalXML(v)
			if err != nil {
				return err
			}
			if changed {
				b.patches = append(b.patches, filePatch{box.payload, clean})
			}
		case "uuid":
			id, err := b.read(box.payload, 16)
			if err != nil {
				return err
			}
			if !bytes.Equal(id, []byte{0xbe, 0x7a, 0xcf, 0xcb, 0x97, 0xa9, 0x42, 0xe8, 0x9c, 0x71, 0x99, 0x94, 0x91, 0xe3, 0xaf, 0xac}) {
				return ErrUnsupported
			}
			v, err := b.read(box.payload+16, box.end-box.payload-16)
			if err != nil {
				return err
			}
			clean, changed, err := scrubRegionalXML(v)
			if err != nil {
				return err
			}
			if changed {
				b.patches = append(b.patches, filePatch{box.payload + 16, clean})
			}
		case "pssh", "sinf", "cmov", "sign", "sigi":
			return ErrUnsupported
		case "hdlr":
			h, err := b.read(box.payload, 12)
			if err != nil {
				return err
			}
			if string(h[8:12]) == "meta" || string(h[8:12]) == "mdta" {
				return ErrUnsupported
			} // Timed telemetry is a track, not optional tags.
		}
	}
	return nil
}

func (b *bmffCleaner) meta(box mediaBox, depth int) error {
	if depth > 24 {
		return ErrUnsupported
	}
	flags, err := b.read(box.payload, 4)
	if err != nil || flags[0] != 0 {
		return ErrUnsupported
	}
	children, err := b.boxes(box.payload+4, box.end)
	if err != nil {
		return err
	}
	var info, loc *mediaBox
	keys := map[string]string{}
	for i, c := range children {
		switch c.kind {
		case "iinf":
			info = &children[i]
		case "iloc":
			loc = &children[i]
		case "keys":
			data, err := b.read(c.payload, c.end-c.payload)
			if err != nil || len(data) < 8 {
				return ErrUnsupported
			}
			count := binary.BigEndian.Uint32(data[4:])
			pos := 8
			if count > 65536 {
				return ErrUnsupported
			}
			for n := uint32(1); n <= count; n++ {
				if len(data)-pos < 8 {
					return ErrUnsupported
				}
				size := int(binary.BigEndian.Uint32(data[pos:]))
				if size < 8 || size > len(data)-pos {
					return ErrUnsupported
				}
				var id [4]byte
				binary.BigEndian.PutUint32(id[:], n)
				keys[string(id[:])] = string(data[pos+8 : pos+size])
				pos += size
			}
			if pos != len(data) {
				return ErrUnsupported
			}
		}
	}
	if info != nil || loc != nil {
		if info == nil || loc == nil {
			return ErrUnsupported
		}
		if err := b.heif(children, *info, *loc); err != nil {
			return err
		}
	}
	for _, c := range children {
		if c.kind != "ilst" {
			continue
		}
		items, err := b.boxes(c.payload, c.end)
		if err != nil {
			return err
		}
		for _, item := range items {
			key := keys[item.kind]
			if item.kind == "----" {
				fields, err := b.boxes(item.payload, item.end)
				if err != nil {
					return err
				}
				for _, f := range fields {
					if f.kind == "name" {
						v, err := b.read(f.payload+4, f.end-f.payload-4)
						if err != nil {
							return err
						}
						key = string(v)
					}
				}
			}
			drop := regionalKey(key) || item.kind == "\xa9xyz" || item.kind == "loci"
			if item.kind == "\xa9day" || strings.Contains(strings.ToLower(key), "date") {
				v, err := b.read(item.payload, item.end-item.payload)
				if err != nil {
					return err
				}
				drop = drop || regionalDate("date", string(v))
			}
			if drop {
				if err := b.free(item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type itemExtent struct {
	id   uint32
	span fileSpan
}

func (b *bmffCleaner) heif(children []mediaBox, info, loc mediaBox) error {
	h, err := b.read(info.payload, 8)
	if err != nil {
		return err
	}
	skip := int64(6)
	if h[0] == 1 {
		skip = 8
	} else if h[0] != 0 {
		return ErrUnsupported
	}
	entries, err := b.boxes(info.payload+skip, info.end)
	if err != nil {
		return err
	}
	declared := uint32(binary.BigEndian.Uint16(h[4:6]))
	if h[0] == 1 {
		declared = binary.BigEndian.Uint32(h[4:8])
	}
	if declared != uint32(len(entries)) {
		return ErrUnsupported
	}
	kinds := map[uint32]string{}
	ids := map[uint32]bool{}
	for _, entry := range entries {
		if entry.kind != "infe" {
			return ErrUnsupported
		}
		v, err := b.read(entry.payload, entry.end-entry.payload)
		if err != nil || len(v) < 12 {
			return ErrUnsupported
		}
		var id uint32
		pos := 0
		switch v[0] {
		case 2:
			id = uint32(binary.BigEndian.Uint16(v[4:]))
			pos = 6
		case 3:
			id = binary.BigEndian.Uint32(v[4:])
			pos = 8
		default:
			return ErrUnsupported
		}
		if len(v)-pos < 6 || binary.BigEndian.Uint16(v[pos:]) != 0 {
			return ErrUnsupported
		}
		if ids[id] {
			return ErrUnsupported
		}
		ids[id] = true
		pos += 2
		typ := string(v[pos : pos+4])
		pos += 4
		if typ == "Exif" {
			kinds[id] = "exif"
		}
		if typ == "mime" {
			parts := bytes.Split(v[pos:], []byte{0})
			if len(parts) < 3 {
				return ErrUnsupported
			}
			mime := string(parts[1])
			if mime == "application/rdf+xml" || mime == "application/xml" || mime == "text/xml" {
				if len(parts[2]) != 0 {
					return ErrUnsupported
				}
				kinds[id] = "xmp"
			}
		}
	}
	data, err := b.read(loc.payload, loc.end-loc.payload)
	if err != nil || len(data) < 8 {
		return ErrUnsupported
	}
	version := data[0]
	if version > 2 {
		return ErrUnsupported
	}
	offsetSize, lengthSize, baseSize, indexSize := int(data[4]>>4), int(data[4]&15), int(data[5]>>4), 0
	if version > 0 {
		indexSize = int(data[5] & 15)
	}
	pos := 6
	read := func(n int) (uint64, error) {
		if n < 0 || n > 8 || n > len(data)-pos {
			return 0, ErrUnsupported
		}
		var v uint64
		for _, c := range data[pos : pos+n] {
			v = v<<8 | uint64(c)
		}
		pos += n
		return v, nil
	}
	countSize := 2
	if version == 2 {
		countSize = 4
	}
	count, err := read(countSize)
	if err != nil || count > 65536 {
		return ErrUnsupported
	}
	var extents []itemExtent
	for i := uint64(0); i < count; i++ {
		idSize := 2
		if version == 2 {
			idSize = 4
		}
		id, err := read(idSize)
		if err != nil {
			return err
		}
		method := uint64(0)
		if version > 0 {
			method, err = read(2)
			if err != nil || method&15 > 1 {
				return ErrUnsupported
			}
			method &= 15
		}
		ref, err := read(2)
		if err != nil || ref != 0 {
			return ErrUnsupported
		}
		base, err := read(baseSize)
		if err != nil {
			return err
		}
		n, err := read(2)
		if err != nil || n > 65536 {
			return ErrUnsupported
		}
		origin := uint64(0)
		if method == 1 {
			found := false
			for _, c := range children {
				if c.kind == "idat" {
					origin = uint64(c.payload)
					found = true
					break
				}
			}
			if !found {
				return ErrUnsupported
			}
		}
		for j := uint64(0); j < n; j++ {
			if indexSize > 0 {
				if _, err := read(indexSize); err != nil {
					return err
				}
			}
			o, err := read(offsetSize)
			if err != nil {
				return err
			}
			length, err := read(lengthSize)
			if err != nil || length == 0 {
				return ErrUnsupported
			}
			if origin > uint64(b.size) || base > uint64(b.size)-origin || o > uint64(b.size)-origin-base {
				return ErrUnsupported
			}
			start := origin + base + o
			if length > uint64(b.size)-start {
				return ErrUnsupported
			}
			extents = append(extents, itemExtent{uint32(id), fileSpan{int64(start), int64(start + length)}})
		}
	}
	if pos != len(data) {
		return ErrUnsupported
	}
	for id, kind := range kinds {
		var selected []fileSpan
		for _, e := range extents {
			if e.id == id {
				selected = append(selected, e.span)
			}
		}
		if len(selected) != 1 {
			return ErrUnsupported
		}
		span := selected[0]
		contained := false
		for _, s := range b.dataSpans {
			if span.start >= s.start && span.end <= s.end {
				contained = true
			}
		}
		for _, c := range children {
			if c.kind == "idat" && span.start >= c.payload && span.end <= c.end {
				contained = true
			}
		}
		if !contained {
			return ErrUnsupported
		}
		for _, e := range extents {
			if e.id != id && span.start < e.span.end && e.span.start < span.end {
				return ErrUnsupported
			}
		}
		v, err := b.read(span.start, span.end-span.start)
		if err != nil {
			return err
		}
		var clean []byte
		var changed bool
		if kind == "exif" {
			if len(v) < 4 {
				return ErrUnsupported
			}
			start := uint64(binary.BigEndian.Uint32(v[:4])) + 4
			if start > uint64(len(v)) {
				return ErrUnsupported
			}
			var out bytes.Buffer
			changed, err = sanitizeTIFF(bytes.NewReader(v[start:]), &out, int64(len(v))-int64(start))
			if err != nil {
				return err
			}
			if changed {
				clean = bytes.Clone(v)
				copy(clean[start:], out.Bytes())
			}
		} else {
			clean, changed, err = scrubRegionalXML(v)
			if err != nil {
				return err
			}
		}
		if changed {
			b.patches = append(b.patches, filePatch{span.start, clean})
		}
	}
	return nil
}
