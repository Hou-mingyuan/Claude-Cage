package transfermeta

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"io"
	"testing"
)

func TestGIFLoop(t *testing.T) {
	frame := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{3, 5}, LoopCount: 7}); err != nil {
		t.Fatal(err)
	}
	data := append([]byte{}, encoded.Bytes()[:encoded.Len()-1]...)
	comment := []byte("GPS=31.23;locale=zh-CN;timezone=Asia/Shanghai")
	data = append(data, 0x21, 0xfe, byte(len(comment)))
	data = append(data, comment...)
	data = append(data, 0, 0x3b)
	clean, err := SanitizeBytes("loop.gif", data)
	if err != nil {
		t.Fatal(err)
	}
	result, err := gif.DecodeAll(bytes.NewReader(clean))
	if err != nil || len(result.Image) != 2 || result.LoopCount != 7 || result.Delay[1] != 5 {
		t.Fatalf("animation changed: %v", err)
	}
	if bytes.Contains(clean, comment) {
		t.Fatal("location comment survived")
	}
}

func TestJPEGAfterScan(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	data = appendJPEGSegment(data[:len(data)-2], 0xfe, []byte("Asia/Shanghai;zh-CN"))
	data = appendJPEGSegment(data, 0xe1, []byte("GPS=31.23"))
	data = append(data, 0xff, 0xd9)
	data = append(data, []byte("trailing location")...)
	clean, err := SanitizeBytes("photo.jpg", data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(clean, []byte("Shanghai")) || bytes.Contains(clean, []byte("GPS")) || bytes.Contains(clean, []byte("trailing")) {
		t.Fatal("post-scan metadata survived")
	}
	before, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	after, err := jpeg.Decode(bytes.NewReader(clean))
	if err != nil {
		t.Fatal(err)
	}
	if !imagesEqual(before, after) {
		t.Fatal("pixels changed")
	}
}

func TestJPEGOrientation(t *testing.T) {
	for v := uint16(1); v <= 8; v++ {
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, testImage(), nil); err != nil {
			t.Fatal(err)
		}
		exif := append(orientationEXIF(v), []byte("GPS=31.23;Asia/Shanghai;zh-CN")...)
		data := appendJPEGSegment(encoded.Bytes()[:2], 0xe1, append([]byte("Exif\x00\x00"), exif...))
		data = append(data, encoded.Bytes()[2:]...)
		clean, err := SanitizeBytes("photo.jpg", data)
		if err != nil {
			t.Fatal(err)
		}
		start := bytes.Index(clean, []byte("Exif\x00\x00"))
		if start < 0 || exifOrientation(clean[start+6:]) != v {
			t.Fatalf("orientation %d lost", v)
		}
		if bytes.Contains(clean, []byte("GPS")) || bytes.Contains(clean, []byte("Shanghai")) {
			t.Fatal("EXIF provenance survived")
		}
		if _, err := jpeg.Decode(bytes.NewReader(clean)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBlockedMedia(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"photo.tiff", []byte{'I', 'I', 42, 0}}, {"renamed.bin", []byte{'M', 'M', 0, 42}},
		{"paper.pdf", []byte("%PDF-1.7")}, {"renamed.txt", []byte("%PDF-1.7")},
		{"photo.heic", []byte("heic")}, {"photo.avif", []byte("avif")}, {"renamed.bin", []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'}},
		{"audio.mp3", []byte("ID3")}, {"renamed.bin", []byte("fLaC")}, {"renamed.bin", []byte("OggS")},
		{"movie.mp4", []byte("video")}, {"renamed.bin", []byte{0x1a, 0x45, 0xdf, 0xa3}},
		{"renamed.bin", []byte("RIFF\x10\x00\x00\x00AVI LIST")}, {"renamed.bin", []byte("FORM\x10\x00\x00\x00AIFFCOMM")},
		{"renamed.bin", []byte("8BPS")}, {"renamed.bin", []byte{'I', 'I', 43, 0}},
	}
	for _, tc := range cases {
		if out, err := SanitizeBytes(tc.name, tc.data); !errors.Is(err, ErrUnsupported) || out != nil {
			t.Errorf("%s escaped byte policy", tc.name)
		}
		if p, err := Prepare(tc.name, bytes.NewReader(tc.data), int64(len(tc.data))); !errors.Is(err, ErrUnsupported) || p != nil {
			t.Errorf("%s escaped stream policy", tc.name)
		}
	}
}

func TestMalformedFailsClosed(t *testing.T) {
	for name, data := range map[string][]byte{
		"image.jpg": {0xff, 0xd8, 0xff, 0xe1, 0, 50}, "image.png": pngSignature,
		"image.gif": []byte("GIF89a"), "image.webp": []byte("RIFF\x10\x00\x00\x00WEBP"),
		"report.docx": []byte("PK\x03\x04broken"),
	} {
		if clean, err := SanitizeBytes(name, data); err == nil || clean != nil {
			t.Errorf("%s fell back to original", name)
		}
		if p, err := Prepare(name, bytes.NewReader(data), int64(len(data))); err == nil || p != nil {
			t.Errorf("%s stream fell back", name)
		}
	}
}

func TestWAVMetadata(t *testing.T) {
	var chunks bytes.Buffer
	writeChunk := func(tag string, payload []byte) {
		chunks.WriteString(tag)
		_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(payload)))
		chunks.Write(payload)
		if len(payload)%2 != 0 {
			chunks.WriteByte(0)
		}
	}
	fmt := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmt, 1)
	binary.LittleEndian.PutUint16(fmt[2:], 1)
	binary.LittleEndian.PutUint32(fmt[4:], 8000)
	binary.LittleEndian.PutUint32(fmt[8:], 16000)
	binary.LittleEndian.PutUint16(fmt[12:], 2)
	binary.LittleEndian.PutUint16(fmt[14:], 16)
	writeChunk("fmt ", fmt)
	for _, tag := range []string{"LIST", "XMP ", "JUNK"} {
		payload := []byte("Asia/Shanghai;zh-CN;GPS=31.23")
		if tag == "LIST" {
			payload = append([]byte("INFO"), payload...)
		}
		writeChunk(tag, payload)
	}
	samples := []byte{1, 2, 3, 4, 5, 6}
	writeChunk("data", samples)
	var data bytes.Buffer
	data.WriteString("RIFF")
	_ = binary.Write(&data, binary.LittleEndian, uint32(4+chunks.Len()))
	data.WriteString("WAVE")
	data.Write(chunks.Bytes())
	clean, err := SanitizeBytes("audio.wav", data.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(clean, []byte("Shanghai")) || !bytes.HasSuffix(clean, samples) || int(binary.LittleEndian.Uint32(clean[4:8])) != len(clean)-8 {
		t.Fatal("metadata or audio corrupted")
	}
}

func TestArchiveMetadata(t *testing.T) {
	var raw bytes.Buffer
	w := tar.NewWriter(&raw)
	data := pngWithMetadata(t)
	if err := w.WriteHeader(&tar.Header{Name: "folder/photo.png", Size: int64(len(data)), Mode: 0600, Uid: 123, Uname: "Shanghai", PAXRecords: map[string]string{"comment": "zh-CN"}}); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(data)
	_ = w.Close()
	p, err := PrepareArchive(&raw)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	r := tar.NewReader(p.Body)
	h, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	clean, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if h.Size != int64(len(clean)) || h.Uid != 0 || h.Uname != "" || len(h.PAXRecords) != 0 || bytes.Contains(clean, []byte("GPS")) {
		t.Fatal("archive provenance survived")
	}
}

func TestEmbeddedUnsupported(t *testing.T) {
	data := makeZip(t, map[string]string{"word/document.xml": "body", "word/media/renamed.bin": "%PDF-1.7 location"})
	if clean, err := SanitizeBytes("report.docx", data); err == nil || clean != nil {
		t.Fatal("unsupported embedded media escaped")
	}
}

func TestZipProvenance(t *testing.T) {
	var raw bytes.Buffer
	w := zip.NewWriter(&raw)
	_ = w.SetComment("Asia/Shanghai")
	h := &zip.FileHeader{Name: "word/document.xml", Comment: "zh-CN", Extra: []byte{0xfe, 0xca, 1, 0, 42}}
	p, err := w.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Write([]byte("body"))
	_ = w.Close()
	clean, err := SanitizeBytes("report.docx", raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	r, err := zip.NewReader(bytes.NewReader(clean), int64(len(clean)))
	if err != nil {
		t.Fatal(err)
	}
	if r.Comment != "" || r.File[0].Comment != "" || bytes.Contains(clean, []byte("Shanghai")) {
		t.Fatal("ZIP provenance survived")
	}
}

func TestSignatureInSource(t *testing.T) {
	data := []byte("package example\nvar magic = \"%PDF-1.7\"\n")
	clean, err := SanitizeBytes("source.go", data)
	if err != nil || !bytes.Equal(clean, data) {
		t.Fatal("source code was mistaken for a PDF")
	}
}

// Files without capture-device metadata must reach the guest unchanged by
// default: archives, legacy documents, icons, and text that merely starts
// with bytes resembling a media signature.
func TestOrdinaryFilesPassThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"archive.zip", []byte("PK\x03\x04archive")}, {"gradle-wrapper.jar", []byte("PK\x03\x04jar")},
		{"archive.tar", []byte("archive")}, {"package.tgz", []byte{0x1f, 0x8b, 8, 0}}, {"renamed.bin", []byte{0x1f, 0x8b}},
		{"logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)}, {"favicon.ico", []byte{0, 0, 1, 0, 1, 0}},
		{"legacy.doc", []byte{0xd0, 0xcf, 0x11, 0xe0}}, {"image.bmp", []byte("BM\x36\x00")},
		{"notes.txt", []byte{0xff, 0xfe, 'h', 0, 'i', 0}}, {"cars.csv", []byte("BMW,320i\n")},
		{"schema.txt", []byte("FORM fields\n")}, {"tags.md", []byte("ID3 tags are text\n")},
		{"script.ps", []byte("%!PS-Adobe")}, {"empty.png", nil},
	} {
		out, err := SanitizeBytes(tc.name, tc.data)
		if err != nil || !bytes.Equal(out, tc.data) {
			t.Errorf("%s: byte policy changed or rejected an ordinary file: %v", tc.name, err)
		}
		p, err := Prepare(tc.name, bytes.NewReader(tc.data), int64(len(tc.data)))
		if err != nil {
			t.Errorf("%s: stream policy rejected an ordinary file: %v", tc.name, err)
			continue
		}
		got, err := io.ReadAll(p.Body)
		if err != nil || !bytes.Equal(got, tc.data) || p.Unclean {
			t.Errorf("%s: stream policy changed an ordinary file", tc.name)
		}
		_ = p.Close()
	}
}
