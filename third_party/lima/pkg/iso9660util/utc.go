// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package iso9660util

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// The writer records the host's local time and UTC offset in every timestamp.
// The guest reads the image, so rewrite each timestamp as the same instant in
// UTC; it then carries no information about the host's time zone.

const isoSector = 2048

type isoImage interface {
	io.ReaderAt
	io.WriterAt
}

// normalizeTimestamps rewrites the volume descriptor dates, directory record
// dates and Rock Ridge TF times of an ISO 9660 image to UTC.
func normalizeTimestamps(f isoImage) error {
	type extent struct{ block, size uint32 }
	var pending []extent
	blockSize := uint32(isoSector)
	for sector := int64(16); ; sector++ {
		vd := make([]byte, isoSector)
		if _, err := f.ReadAt(vd, sector*isoSector); err != nil {
			return fmt.Errorf("read volume descriptor: %w", err)
		}
		if string(vd[1:6]) != "CD001" {
			return errors.New("invalid ISO 9660 volume descriptor")
		}
		if vd[0] == 255 {
			break
		}
		if vd[0] != 1 && vd[0] != 2 {
			continue
		}
		for _, at := range []int{813, 830, 847, 864} {
			if err := utcDecimalTime(vd[at : at+17]); err != nil {
				return err
			}
		}
		root := vd[156 : 156+34]
		utcRecordTime(root[18:25])
		if size := uint32(binary.LittleEndian.Uint16(vd[128:130])); size != 0 {
			blockSize = size
		}
		pending = append(pending, extent{binary.LittleEndian.Uint32(root[2:6]), binary.LittleEndian.Uint32(root[10:14])})
		if _, err := f.WriteAt(vd, sector*isoSector); err != nil {
			return err
		}
		if sector > 64 {
			return errors.New("too many ISO 9660 volume descriptors")
		}
	}
	seen := map[uint32]bool{}
	for len(pending) > 0 {
		dir := pending[0]
		pending = pending[1:]
		if seen[dir.block] || dir.size == 0 || dir.size > 64<<20 {
			continue
		}
		seen[dir.block] = true
		data := make([]byte, dir.size)
		if _, err := f.ReadAt(data, int64(dir.block)*int64(blockSize)); err != nil {
			return fmt.Errorf("read directory: %w", err)
		}
		for at := 0; at < len(data); {
			length := int(data[at])
			if length == 0 {
				// Records never cross a sector boundary; padding fills the rest.
				at = (at/int(blockSize) + 1) * int(blockSize)
				continue
			}
			if length < 34 || at+length > len(data) {
				return errors.New("invalid ISO 9660 directory record")
			}
			record := data[at : at+length]
			utcRecordTime(record[18:25])
			nameLength := int(record[32])
			if record[25]&2 != 0 && !(nameLength == 1 && record[33] <= 1) {
				pending = append(pending, extent{binary.LittleEndian.Uint32(record[2:6]), binary.LittleEndian.Uint32(record[10:14])})
			}
			use := 33 + nameLength
			if nameLength%2 == 0 {
				use++
			}
			if use < length {
				if err := utcSystemUse(f, record[use:], blockSize, 0); err != nil {
					return err
				}
			}
			at += length
		}
		if _, err := f.WriteAt(data, int64(dir.block)*int64(blockSize)); err != nil {
			return err
		}
	}
	return nil
}

// utcSystemUse rewrites Rock Ridge TF entries and follows continuation areas.
func utcSystemUse(f isoImage, area []byte, blockSize uint32, depth int) error {
	if depth > 8 {
		return errors.New("too many ISO 9660 continuation areas")
	}
	for at := 0; at+4 <= len(area); {
		signature, length := string(area[at:at+2]), int(area[at+2])
		if length < 4 || at+length > len(area) || signature == "ST" {
			return nil
		}
		entry := area[at : at+length]
		switch signature {
		case "TF":
			size := 7
			if entry[4]&0x80 != 0 {
				size = 17
			}
			for i, offset := 0, 5; i < 7; i++ {
				if entry[4]&(1<<i) == 0 {
					continue
				}
				if offset+size > len(entry) {
					return errors.New("invalid Rock Ridge TF entry")
				}
				if size == 7 {
					utcRecordTime(entry[offset : offset+7])
				} else if err := utcDecimalTime(entry[offset : offset+17]); err != nil {
					return err
				}
				offset += size
			}
		case "CE":
			if length < 28 {
				return errors.New("invalid continuation entry")
			}
			block := binary.LittleEndian.Uint32(entry[4:8])
			offset := binary.LittleEndian.Uint32(entry[12:16])
			size := binary.LittleEndian.Uint32(entry[20:24])
			if size > isoSector {
				return errors.New("invalid continuation area")
			}
			position := int64(block)*int64(blockSize) + int64(offset)
			continuation := make([]byte, size)
			if _, err := f.ReadAt(continuation, position); err != nil {
				return err
			}
			if err := utcSystemUse(f, continuation, blockSize, depth+1); err != nil {
				return err
			}
			if _, err := f.WriteAt(continuation, position); err != nil {
				return err
			}
		}
		at += length
	}
	return nil
}

// utcRecordTime rewrites a 7-byte directory record time.
func utcRecordTime(b []byte) {
	if b[0] == 0 && b[1] == 0 && b[2] == 0 {
		return
	}
	zone := time.FixedZone("", int(int8(b[6]))*15*60)
	t := time.Date(int(b[0])+1900, time.Month(b[1]), int(b[2]), int(b[3]), int(b[4]), int(b[5]), 0, zone).UTC()
	b[0], b[1], b[2] = byte(t.Year()-1900), byte(t.Month()), byte(t.Day())
	b[3], b[4], b[5], b[6] = byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0
}

// utcDecimalTime rewrites a 17-byte volume descriptor time.
func utcDecimalTime(b []byte) error {
	digits := strings.ReplaceAll(string(b[:16]), " ", "0")
	if digits == "0000000000000000" || digits == string(make([]byte, 16)) {
		b[16] = 0
		return nil
	}
	n := func(from, to int) (int, error) { return strconv.Atoi(digits[from:to]) }
	var v [7]int
	for i, r := range [][2]int{{0, 4}, {4, 6}, {6, 8}, {8, 10}, {10, 12}, {12, 14}, {14, 16}} {
		x, err := n(r[0], r[1])
		if err != nil {
			return fmt.Errorf("invalid ISO 9660 date %q", digits)
		}
		v[i] = x
	}
	zone := time.FixedZone("", int(int8(b[16]))*15*60)
	t := time.Date(v[0], time.Month(v[1]), v[2], v[3], v[4], v[5], v[6]*1e7, zone).UTC()
	copy(b, fmt.Sprintf("%04d%02d%02d%02d%02d%02d%02d", t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1e7))
	b[16] = 0
	return nil
}
