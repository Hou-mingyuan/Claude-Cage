package transfermeta

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"strings"
	"testing"
)

func testImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	img.Set(1, 0, color.RGBA{0, 255, 0, 255})
	img.Set(0, 1, color.RGBA{0, 0, 255, 255})
	img.Set(1, 1, color.RGBA{255, 255, 0, 255})
	return img
}

func appendJPEGSegment(data []byte, marker byte, payload []byte) []byte {
	out := append([]byte{}, data...)
	segment := []byte{0xFF, marker, byte((len(payload) + 2) >> 8), byte((len(payload) + 2) & 0xFF)}
	segment = append(segment, payload...)
	out = append(out, segment...)
	return out
}

func jpegWithMetadata(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	out := append([]byte{}, data[:2]...)
	out = appendJPEGSegment(out, 0xE1, []byte("Exif\x00\x00GPSLatitude=31.23;OffsetTimeOriginal=+08:00;Language=zh-CN"))
	out = appendJPEGSegment(out, 0xFE, []byte("timezone=Asia/Shanghai location=Shanghai"))
	out = append(out, data[2:]...)
	return out
}

func pngChunk(chunkType string, payload []byte) []byte {
	out := make([]byte, 8+len(payload)+4)
	binary.BigEndian.PutUint32(out[:4], uint32(len(payload)))
	copy(out[4:8], chunkType)
	copy(out[8:8+len(payload)], payload)
	binary.BigEndian.PutUint32(out[8+len(payload):], crc32.ChecksumIEEE(out[4:8+len(payload)]))
	return out
}

func pngWithMetadata(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, testImage()); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	out := append([]byte{}, data[:len(data)-12]...)
	out = append(out, pngChunk("tEXt", []byte("Language\x00zh-CN\x00GPSLatitude=31.23"))...)
	out = append(out, pngChunk("eXIf", []byte("GPSLatitude=31.23"))...)
	out = append(out, pngChunk("tIME", []byte{0x07, 0xE8, 0x0B, 0x0F, 0x0A, 0x00, 0x00})...)
	out = append(out, data[len(data)-12:]...)
	return out
}

func gifWithMetadata(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := gif.Encode(&encoded, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	comment := []byte("GPSLatitude=31.23 language=zh-CN")
	out := append([]byte{}, data[:len(data)-1]...)
	out = append(out, 0x21, 0xFE, byte(len(comment)))
	out = append(out, comment...)
	out = append(out, 0)
	out = append(out, 0x3B)
	return out
}

func webpWithMetadata() []byte {
	var out bytes.Buffer
	chunks := []struct {
		name    string
		payload []byte
	}{
		{"VP8X", []byte{0x0C, 0, 0, 0, 1, 0, 0, 1, 0, 0}},
		{"VP8 ", []byte{1, 2, 3, 4}},
		{"EXIF", []byte("GPSLatitude=31.23")},
		{"XMP ", []byte("zh-CN")},
	}
	size := 4
	for _, chunk := range chunks {
		size += 8 + len(chunk.payload) + len(chunk.payload)%2
	}
	out.WriteString("RIFF")
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(size))
	out.Write(header[:])
	out.WriteString("WEBP")
	for _, chunk := range chunks {
		out.WriteString(chunk.name)
		binary.LittleEndian.PutUint32(header[:], uint32(len(chunk.payload)))
		out.Write(header[:])
		out.Write(chunk.payload)
		if len(chunk.payload)%2 == 1 {
			out.WriteByte(0)
		}
	}
	return out.Bytes()
}

func makeZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for name, content := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func readZipEntries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	entries := make(map[string][]byte)
	for _, entry := range reader.File {
		part, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries[entry.Name] = content
	}
	return entries
}

func TestJPEGMetadataRemovedWithoutChangingPixels(t *testing.T) {
	original := jpegWithMetadata(t)
	cleaned, err := SanitizeBytes("photo.jpg", original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cleaned, original) {
		t.Fatal("metadata was not removed")
	}
	for _, value := range []string{"GPSLatitude", "OffsetTimeOriginal", "zh-CN", "Asia/Shanghai"} {
		if bytes.Contains(cleaned, []byte(value)) {
			t.Fatalf("metadata %q survived", value)
		}
	}
	before, err := jpeg.Decode(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	after, err := jpeg.Decode(bytes.NewReader(cleaned))
	if err != nil {
		t.Fatal(err)
	}
	if !imagesEqual(before, after) {
		t.Fatal("pixels changed")
	}
}

func TestPNGMetadataRemovedWithoutChangingPixels(t *testing.T) {
	original := pngWithMetadata(t)
	cleaned, err := SanitizeBytes("photo.png", original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cleaned, original) {
		t.Fatal("metadata was not removed")
	}
	for _, value := range []string{"GPSLatitude", "zh-CN"} {
		if bytes.Contains(cleaned, []byte(value)) {
			t.Fatalf("metadata %q survived", value)
		}
	}
	before, err := png.Decode(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	after, err := png.Decode(bytes.NewReader(cleaned))
	if err != nil {
		t.Fatal(err)
	}
	if !imagesEqual(before, after) {
		t.Fatal("pixels changed")
	}
}

func TestGIFMetadataRemovedWithoutChangingFrames(t *testing.T) {
	original := gifWithMetadata(t)
	cleaned, err := SanitizeBytes("animation.gif", original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cleaned, original) {
		t.Fatal("metadata was not removed")
	}
	if bytes.Contains(cleaned, []byte("GPSLatitude")) {
		t.Fatal("comment metadata survived")
	}
	before, err := gif.DecodeAll(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	after, err := gif.DecodeAll(bytes.NewReader(cleaned))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Image) != len(after.Image) || !imagesEqual(before.Image[0], after.Image[0]) {
		t.Fatal("frames changed")
	}
}

func TestWebPMetadataRemovedWithoutChangingImageChunks(t *testing.T) {
	original := webpWithMetadata()
	cleaned, err := SanitizeBytes("photo.webp", original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cleaned, original) {
		t.Fatal("metadata was not removed")
	}
	if bytes.Contains(cleaned, []byte("GPSLatitude")) || bytes.Contains(cleaned, []byte("zh-CN")) {
		t.Fatal("metadata survived")
	}
	if !bytes.Contains(cleaned, []byte("VP8 ")) {
		t.Fatal("image chunk removed")
	}
	index := bytes.Index(cleaned, []byte("VP8X"))
	if index < 0 || index+8 >= len(cleaned) {
		t.Fatal("VP8X header removed")
	}
	if cleaned[index+8]&0x0C != 0 {
		t.Fatal("VP8X still advertises removed EXIF/XMP chunks")
	}
	if got := int(binary.LittleEndian.Uint32(cleaned[4:8])); got != len(cleaned)-8 {
		t.Fatalf("RIFF size is %d, file body is %d", got, len(cleaned)-8)
	}
}

func TestOOXMLMetadataRemovedWithoutChangingDocumentContent(t *testing.T) {
	original := makeZip(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"docProps/core.xml":   `<?xml version="1.0"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/"><dc:title>Report</dc:title><dc:creator>Sandro</dc:creator><cp:lastModifiedBy>Sandro</cp:lastModifiedBy><dcterms:created>2026-11-15T10:00:00+08:00</dcterms:created><dc:language>zh-CN</dc:language></cp:coreProperties>`,
		"docProps/app.xml":    `<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"><Company>Shanghai</Company><Application>Word</Application></Properties>`,
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p>Hello</w:p></w:body></w:document>`,
	})
	cleaned, err := SanitizeBytes("report.docx", original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cleaned, original) {
		t.Fatal("metadata was not removed")
	}
	entries := readZipEntries(t, cleaned)
	for _, value := range []string{"zh-CN", "Sandro", "2026-11-15", "Shanghai"} {
		for name, content := range entries {
			if name == "word/document.xml" {
				continue
			}
			if bytes.Contains(content, []byte(value)) {
				t.Fatalf("%q survived in %s", value, name)
			}
		}
	}
	if string(entries["word/document.xml"]) != `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p>Hello</w:p></w:body></w:document>` {
		t.Fatal("document content changed")
	}
	if !bytes.Contains(entries["docProps/core.xml"], []byte("coreProperties")) {
		t.Fatal("core properties root removed")
	}
}

func TestODFMetadataRemovedWithoutChangingContent(t *testing.T) {
	original := makeZip(t, map[string]string{
		"mimetype":              "application/vnd.oasis.opendocument.text",
		"META-INF/manifest.xml": `<?xml version="1.0"?><manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0"><manifest:file-entry manifest:full-path="meta.xml" manifest:media-type="text/xml"/></manifest:manifest>`,
		"meta.xml":              `<?xml version="1.0"?><office:document-meta xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:meta="urn:oasis:names:tc:opendocument:xmlns:meta:1.0"><office:meta><dc:creator>Sandro</dc:creator><dc:language>zh-CN</dc:language><meta:creation-date>2026-11-15T10:00:00+08:00</meta:creation-date></office:meta></office:document-meta>`,
		"settings.xml":          `<?xml version="1.0"?><office:document-settings xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:config="urn:oasis:names:tc:opendocument:xmlns:config:1.0"><office:settings><config:config-item-set config:name="settings"><config:config-item config:name="Language" config:type="string">zh-CN</config:config-item></config:config-item-set></office:settings></office:document-settings>`,
		"content.xml":           `<?xml version="1.0"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"><office:body><office:text><text:p>Hello</text:p></office:text></office:body></office:document-content>`,
	})
	cleaned, err := SanitizeBytes("notes.odt", original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cleaned, original) {
		t.Fatal("metadata was not removed")
	}
	entries := readZipEntries(t, cleaned)
	for _, value := range []string{"zh-CN", "Sandro", "2026-11-15"} {
		if bytes.Contains(entries["meta.xml"], []byte(value)) {
			t.Fatalf("%q survived", value)
		}
	}
	if !bytes.Contains(entries["settings.xml"], []byte("zh-CN")) {
		t.Fatal("functional ODF language settings changed")
	}
	if !bytes.Contains(entries["content.xml"], []byte("Hello")) {
		t.Fatal("content removed")
	}
	if !bytes.Contains(entries["meta.xml"], []byte("meta")) {
		t.Fatal("meta root removed")
	}
}

func TestUnknownFormatIsPassedThrough(t *testing.T) {
	data := []byte("ordinary video bytes")
	prepared, err := Prepare("ordinary.bin", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	got, err := io.ReadAll(prepared.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) || prepared.Size != int64(len(data)) {
		t.Fatal("unknown file changed")
	}
}

func TestPreparedJPEGRemovesMetadataAndUsesNewSize(t *testing.T) {
	original := jpegWithMetadata(t)
	file, err := os.CreateTemp(t.TempDir(), "source-*.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(original); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare("photo.jpg", file, int64(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	cleaned, err := io.ReadAll(prepared.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cleaned, original) || prepared.Size != int64(len(cleaned)) {
		t.Fatal("prepared body did not use cleaned size")
	}
	if bytes.Contains(cleaned, []byte("GPSLatitude")) {
		t.Fatal("metadata survived")
	}
}

func imagesEqual(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			ar, ag, ab, aa := a.At(x, y).RGBA()
			br, bg, bb, ba := b.At(x, y).RGBA()
			if ar != br || ag != bg || ab != bb || aa != ba {
				return false
			}
		}
	}
	return true
}

func TestZipWithoutMetadataIsUnchanged(t *testing.T) {
	original := makeZip(t, map[string]string{"word/document.xml": "hello"})
	cleaned, err := SanitizeBytes("report.docx", original)
	if err != nil {
		t.Fatal(err)
	}
	if string(readZipEntries(t, cleaned)["word/document.xml"]) != "hello" {
		t.Fatal("document content changed")
	}
}

func TestTextContentIsNotTreatedAsMetadata(t *testing.T) {
	data := []byte("Timezone: Asia/Shanghai\nLanguage: zh-CN\nGPS: 31.23,121.47\n")
	cleaned, err := SanitizeBytes("notes.txt", data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cleaned, data) {
		t.Fatal("text content changed")
	}
	if !strings.Contains(string(cleaned), "Asia/Shanghai") {
		t.Fatal("text content removed")
	}
}
