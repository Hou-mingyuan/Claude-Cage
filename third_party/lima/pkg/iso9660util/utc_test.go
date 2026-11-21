// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package iso9660util

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"gotest.tools/v3/assert"
)

// isoTimes collects every timestamp the guest can read from the image.
type isoTimes struct {
	decimal []string // 17-byte volume descriptor dates
	record  [][]byte // 7-byte directory record and Rock Ridge times
}

func readISOTimes(t *testing.T, image []byte) isoTimes {
	t.Helper()
	var out isoTimes
	vd := image[16*isoSector : 17*isoSector]
	assert.Equal(t, string(vd[1:6]), "CD001")
	for _, at := range []int{813, 830, 847, 864} {
		out.decimal = append(out.decimal, string(vd[at:at+17]))
	}
	blockSize := int(binary.LittleEndian.Uint16(vd[128:130]))
	type extent struct{ block, size int }
	pending := []extent{{int(binary.LittleEndian.Uint32(vd[158:162])), int(binary.LittleEndian.Uint32(vd[166:170]))}}
	seen := map[int]bool{}
	tf := 0
	for len(pending) > 0 {
		dir := pending[0]
		pending = pending[1:]
		if seen[dir.block] {
			continue
		}
		seen[dir.block] = true
		data := image[dir.block*blockSize : dir.block*blockSize+dir.size]
		for at := 0; at < len(data); {
			length := int(data[at])
			if length == 0 {
				at = (at/blockSize + 1) * blockSize
				continue
			}
			record := data[at : at+length]
			out.record = append(out.record, record[18:25])
			nameLength := int(record[32])
			if record[25]&2 != 0 && !(nameLength == 1 && record[33] <= 1) {
				pending = append(pending, extent{int(binary.LittleEndian.Uint32(record[2:6])), int(binary.LittleEndian.Uint32(record[10:14]))})
			}
			use := 33 + nameLength + 1 - nameLength%2
			for area := record[use:]; len(area) >= 4 && area[2] >= 4 && int(area[2]) <= len(area); area = area[area[2]:] {
				if string(area[:2]) == "TF" {
					tf++
					for i, offset := 0, 5; i < 7; i++ {
						if area[4]&(1<<i) != 0 {
							out.record = append(out.record, area[offset:offset+7])
							offset += 7
						}
					}
				}
			}
			at += length
		}
	}
	assert.Assert(t, tf > 0, "the image has no Rock Ridge times to check")
	return out
}

func TestWriteRecordsOnlyUTCTimes(t *testing.T) {
	// A host east of UTC; the guest must not learn this offset.
	local := time.Local
	time.Local = time.FixedZone("host", 8*60*60)
	t.Cleanup(func() { time.Local = local })

	path := filepath.Join(t.TempDir(), "cidata.iso")
	before := time.Now().UTC().Add(-2 * time.Second)
	err := Write(path, "cidata", []Entry{
		{Path: "user-data", Reader: strings.NewReader("#cloud-config\n")},
		{Path: "meta-data", Reader: strings.NewReader("instance-id: iid-1\n")},
		{Path: "provision.system/00-setup", Reader: strings.NewReader("#!/bin/sh\n")},
	})
	assert.NilError(t, err)
	after := time.Now().UTC().Add(2 * time.Second)
	image, err := os.ReadFile(path)
	assert.NilError(t, err)

	times := readISOTimes(t, image)
	assert.Assert(t, len(times.record) >= 8, "found %d record times", len(times.record))
	for _, d := range times.decimal {
		assert.Equal(t, d[16], byte(0), "volume descriptor date %q keeps an offset", d[:16])
		if d[:16] == "0000000000000000" || d[:16] == string(make([]byte, 16)) {
			continue
		}
		v, err := time.Parse("20060102150405", d[:14])
		assert.NilError(t, err)
		assert.Assert(t, !v.Before(before.Truncate(time.Second)) && !v.After(after), "volume date %s is not the UTC creation time", v)
	}
	for _, r := range times.record {
		assert.Equal(t, r[6], byte(0), "record time %v keeps an offset", r)
		v := time.Date(int(r[0])+1900, time.Month(r[1]), int(r[2]), int(r[3]), int(r[4]), int(r[5]), 0, time.UTC)
		assert.Assert(t, !v.Before(before.Truncate(time.Second)) && !v.After(after), "record time %s is not the UTC creation time", v)
	}

	// The rewritten image stays readable and keeps its files.
	f, err := os.Open(path)
	assert.NilError(t, err)
	defer f.Close()
	info, err := f.Stat()
	assert.NilError(t, err)
	fs, err := iso9660.Read(file.New(f, true), info.Size(), 0, 0)
	assert.NilError(t, err)
	assert.Equal(t, strings.TrimRight(fs.Label(), " \x00"), "cidata")
	entry, err := fs.OpenFile("/provision.system/00-setup", os.O_RDONLY)
	assert.NilError(t, err)
	content, err := io.ReadAll(entry)
	assert.NilError(t, err)
	assert.Equal(t, string(content), "#!/bin/sh\n")
}

func TestUTCTimeConversion(t *testing.T) {
	record := []byte{126, 1, 1, 3, 30, 0, 32} // 2026-01-01 03:30 at +08:00
	utcRecordTime(record)
	assert.DeepEqual(t, record, []byte{125, 12, 31, 19, 30, 0, 0})
	offset := int8(-20)
	decimal := append([]byte("2026010103300012"), byte(offset)) // at -05:00
	assert.NilError(t, utcDecimalTime(decimal))
	assert.Equal(t, string(decimal[:16]), "2026010108300012")
	assert.Equal(t, decimal[16], byte(0))
	empty := append([]byte("0000000000000000"), 0)
	assert.NilError(t, utcDecimalTime(empty))
	assert.Equal(t, string(empty[:16]), "0000000000000000")
}
