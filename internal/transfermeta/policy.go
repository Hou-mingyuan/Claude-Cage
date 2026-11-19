package transfermeta

import (
	"bytes"
	"errors"
	"path"
	"strings"
)

var (
	ErrUnsupported = errors.New("此格式暂不能安全清理元数据。可在设置→通用中明确开启“允许未支持格式原样传递”；原样文件可能保留定位、时区和地区元数据。")
	ErrMalformed   = errors.New("无法在保持文件可用的前提下安全清理。可在设置→通用中明确允许原样传递，元数据可能保留。")
)

// blockedFormat reports media that commonly carries capture-device metadata
// (GPS, time-zone offsets, regional tags) but that this package cannot clean.
// Only distinctive signatures are used, so renaming such a file does not turn
// it into a pass-through file, while ordinary text, code, fonts, icons and
// general-purpose archives are never mistaken for it. Archives, legacy
// documents, SVG/BMP/ICO and other files without such capture metadata pass
// through unchanged, as any other attachment does.
func blockedFormat(name string, head []byte) bool {
	for _, sig := range [][]byte{
		[]byte("8BPS"),                       // Photoshop
		{'I', 'I', 43, 0}, {'M', 'M', 0, 43}, // BigTIFF
		{0x1a, 0x45, 0xdf, 0xa3}, // Matroska/WebM
		[]byte("OggS"),           // Ogg/Opus
		{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11}, // ASF/WMV/WMA
		[]byte("FLV\x01"),
		[]byte("RF64"),
		{0, 0, 0, 0x0c, 'j', 'P', ' ', ' '}, // JPEG 2000
		{0, 0, 0, 0x0c, 'J', 'X', 'L', ' '}, // JPEG XL container
	} {
		if bytes.HasPrefix(head, sig) {
			return true
		}
	}
	if len(head) >= 12 {
		switch string(head[:4]) + string(head[8:12]) {
		case "RIFFAVI ", "RIFFCDXA", "FORMAIFF", "FORMAIFC":
			return true
		}
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".tif", ".tiff", ".heic", ".heif", ".hif", ".avif", ".avifs", ".jxl", ".jp2", ".psd", ".dng", ".raw", ".cr2", ".cr3", ".nef", ".arw", ".orf", ".rw2", ".raf",
		".mp3", ".mp2", ".aac", ".m4a", ".m4b", ".flac", ".ogg", ".oga", ".opus", ".aif", ".aiff", ".wma", ".ape", ".caf", ".amr",
		".mp4", ".m4v", ".mov", ".avi", ".mkv", ".mka", ".webm", ".wmv", ".mpg", ".mpeg", ".mts", ".m2ts", ".3gp", ".3g2", ".flv", ".vob":
		return true
	}
	return false
}

// id3v2Header accepts only a well-formed ID3v2 header, so a text file that
// happens to start with "ID3" is not parsed as MP3.
func id3v2Header(head []byte) bool {
	if len(head) < 10 || string(head[:3]) != "ID3" || head[3] < 2 || head[3] > 4 || head[4] == 0xff {
		return false
	}
	for _, b := range head[6:10] {
		if b&0x80 != 0 {
			return false
		}
	}
	return true
}
