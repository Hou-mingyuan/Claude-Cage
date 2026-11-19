package transfermeta

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"golang.org/x/image/tiff"
)

func regionalXMP() []byte {
	return []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:exif="http://ns.adobe.com/exif/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/" exif:GPSLatitude="31,23N" exif:OffsetTimeOriginal="+08:00" xml:lang="zh-CN"><exif:GPSLongitude>121,47E</exif:GPSLongitude><dc:language>zh-CN</dc:language><dc:title>Readable title</dc:title></rdf:Description></rdf:RDF></x:xmpmeta>`)
}

func TestRegionalXMLPreservesLanguage(t *testing.T) {
	data := regionalXMP()
	out, changed, err := scrubRegionalXML(data)
	if err != nil || !changed || len(out) != len(data) {
		t.Fatal("XMP rewrite failed", err)
	}
	for _, bad := range []string{"31,23N", "121,47E", "+08:00"} {
		if bytes.Contains(out, []byte(bad)) {
			t.Fatal("regional value remained")
		}
	}
	for _, keep := range []string{`xml:lang="zh-CN"`, `Readable title`} {
		if !bytes.Contains(out, []byte(keep)) {
			t.Fatal("functional/language data changed")
		}
	}
	if _, again, err := scrubRegionalXML(out); err != nil || again {
		t.Fatal("cleaned XMP is invalid or not stable", err)
	}
}

func TestRegionalKeyDoesNotMatchDisplayFields(t *testing.T) {
	for _, key := range []string{"Opacity", "Capacity", "Language", "Locale", "Encoding", "ColorSpace", "Orientation", "com.apple.streaming.transportStreamTimestamp"} {
		if regionalKey(key) {
			t.Fatal("functional field mistaken for region", key)
		}
	}
	for _, key := range []string{"GPS_LATITUDE", "CountryCode", "Time_Zone", "com.apple.quicktime.location.ISO6709"} {
		if !regionalKey(key) {
			t.Fatal("known regional field missed", key)
		}
	}
}

func testPDF(signed bool) []byte {
	content := "BT /F1 12 Tf 30 40 Td (Readable text) Tj ET"
	xmp := regionalXMP()
	objects := []string{
		`<< /Type /Catalog /Pages 2 0 R /Lang (zh-CN) /Metadata 7 0 R >>`,
		`<< /Type /Pages /Count 1 /Kids [3 0 R] >>`,
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>`,
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		`<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>`,
		`<< /Title (Readable title) /GPSLatitude (31,23N) /CountryCode (CN) /CreationDate (D:20261116010203+08'00') >>`,
		fmt.Sprintf("<< /Type /Metadata /Subtype /XML /Length %d >>\nstream\n%s\nendstream", len(xmp), xmp),
	}
	if signed {
		objects = append(objects, `<< /Type /Sig /ByteRange [0 1 2 3] /Contents <00> >>`)
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	var offsets []int
	for i, obj := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info 6 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func TestPDFContentAndLanguage(t *testing.T) {
	clean, err := SanitizeBytes("document.pdf", testPDF(false))
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := api.ReadContext(context.Background(), bytes.NewReader(clean), model.NewStatelessConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if err := api.ValidateContext(context.Background(), ctx); err != nil {
		t.Fatal(err)
	}
	root, err := ctx.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	lang, err := root.StringOrHexLiteralEntry("Lang")
	if err != nil || lang == nil || *lang != "zh-CN" {
		t.Fatal("PDF language changed")
	}
	page, _, _, err := ctx.PageDict(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	content, err := ctx.PageContent(page, 1)
	if err != nil || string(content) != "BT /F1 12 Tf 30 40 Td (Readable text) Tj ET" {
		t.Fatal("PDF text/font instructions changed", err)
	}
	info, err := ctx.DereferenceDict(*ctx.Info)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GPSLatitude", "CountryCode", "CreationDate", "ModDate"} {
		if info[key] != nil {
			t.Fatal("regional property or new timestamp remained", key)
		}
	}
	title, _ := info.StringOrHexLiteralEntry("Title")
	if title == nil || *title != "Readable title" {
		t.Fatal("title changed")
	}
	sd, _, err := ctx.DereferenceStreamDict(root["Metadata"])
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.DecodeStream(sd); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sd.Content, []byte("31,23N")) || !bytes.Contains(sd.Content, []byte("zh-CN")) {
		t.Fatal("XMP region/language handling failed")
	}
}

func TestSignedPDFExplicitOriginal(t *testing.T) {
	data := testPDF(true)
	if _, err := SanitizeBytes("signed.pdf", data); err == nil {
		t.Fatal("signed PDF was rewritten")
	}
	out, err := SanitizeBytes("signed.pdf", data, Options{AllowUnclean: true})
	if err != nil || !bytes.Equal(out, data) {
		t.Fatal("signature bytes changed")
	}
}

func tiffWithGPS(t *testing.T) []byte {
	t.Helper()
	var base bytes.Buffer
	if err := tiff.Encode(&base, testImage(), &tiff.Options{Compression: tiff.Deflate}); err != nil {
		t.Fatal(err)
	}
	data := bytes.Clone(base.Bytes())
	order := binary.LittleEndian
	old := int(order.Uint32(data[4:]))
	count := int(order.Uint16(data[old:]))
	var entries [][]byte
	for i := 0; i < count; i++ {
		entries = append(entries, bytes.Clone(data[old+2+i*12:old+2+(i+1)*12]))
	}
	root := len(data)
	rootSize := 2 + (count+3)*12 + 4
	gps := root + rootSize
	exif := gps + 2 + 2*12 + 4
	coords := exif + 2 + 12 + 4
	xmp := coords + 24
	entry := func(tag, typ uint16, n, off uint32) []byte {
		e := make([]byte, 12)
		order.PutUint16(e, tag)
		order.PutUint16(e[2:], typ)
		order.PutUint32(e[4:], n)
		order.PutUint32(e[8:], off)
		return e
	}
	xmpData := regionalXMP()
	entries = append(entries, entry(0x8825, 4, 1, uint32(gps)), entry(0x8769, 4, 1, uint32(exif)), entry(700, 1, uint32(len(xmpData)), uint32(xmp)))
	sort.Slice(entries, func(i, j int) bool { return order.Uint16(entries[i]) < order.Uint16(entries[j]) })
	dir := make([]byte, rootSize)
	order.PutUint16(dir, uint16(len(entries)))
	for i, e := range entries {
		copy(dir[2+i*12:], e)
	}
	data = append(data, dir...)
	dir = make([]byte, 2+2*12+4)
	order.PutUint16(dir, 2)
	copy(dir[2:], entry(1, 2, 2, uint32('N')))
	copy(dir[14:], entry(2, 5, 3, uint32(coords)))
	data = append(data, dir...)
	dir = make([]byte, 2+12+4)
	order.PutUint16(dir, 1)
	copy(dir[2:], entry(0x9011, 2, 7, uint32(xmp+len(xmpData))))
	data = append(data, dir...)
	for _, v := range []uint32{31, 1, 23, 1, 0, 1} {
		data = binary.LittleEndian.AppendUint32(data, v)
	}
	data = append(data, xmpData...)
	data = append(data, []byte("+08:00\x00")...)
	order.PutUint32(data[4:], uint32(root))
	return data
}

func TestTIFFPixelsAndOffsets(t *testing.T) {
	data := tiffWithGPS(t)
	before, err := tiff.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	clean, err := SanitizeBytes("scan.tiff", data)
	if err != nil {
		t.Fatal(err)
	}
	after, err := tiff.Decode(bytes.NewReader(clean))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(clean) || !imagesEqual(before, after) {
		t.Fatal("TIFF pixels/offsets changed")
	}
	for _, v := range []string{"31,23N", "121,47E", "+08:00"} {
		if bytes.Contains(clean, []byte(v)) {
			t.Fatal("TIFF location remained")
		}
	}
	if !bytes.Contains(clean, []byte(`xml:lang="zh-CN"`)) {
		t.Fatal("TIFF XMP language changed")
	}
}

func box(tag string, payload ...[]byte) []byte {
	n := 8
	for _, p := range payload {
		n += len(p)
	}
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, uint32(n))
	copy(out[4:], tag)
	for _, p := range payload {
		out = append(out, p...)
	}
	return out
}

func TestMP4OffsetsAndLanguage(t *testing.T) {
	media := box("mdat", []byte("encoded audio video sample bytes"))
	keys := append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, box("mdta", []byte("com.apple.quicktime.location.ISO6709"))...)
	entry := box(string([]byte{0, 0, 0, 1}), box("data", append(make([]byte, 8), []byte("+31.2300+121.4700/")...)))
	mdhd := box("mdhd", make([]byte, 20), []byte{0x15, 0xc7, 0, 0})
	elng := box("elng", []byte{0, 0, 0, 0}, []byte("zh-CN\x00"))
	data := append(box("ftyp", []byte("isom\x00\x00\x00\x00isom")), media...)
	data = append(data, box("moov", box("trak", box("mdia", mdhd, elng)), box("udta", box("meta", make([]byte, 4), box("keys", keys), box("ilst", entry))))...)
	clean, err := SanitizeBytes("video.mp4", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(clean) || !bytes.Contains(clean, media) || !bytes.Contains(clean, mdhd) || !bytes.Contains(clean, elng) || bytes.Contains(clean, []byte("+31.2300")) {
		t.Fatal("MP4 payload/selection language/offsets changed or GPS remained")
	}
}

func TestHEIFMetadataExtents(t *testing.T) {
	for _, brand := range []string{"heic", "avif"} {
		t.Run(brand, func(t *testing.T) {
			xmp := regionalXMP()
			pixels := []byte("opaque coded image and alpha bytes")
			infe := box("infe", []byte{2, 0, 0, 0, 0, 1, 0, 0}, []byte("mime"), []byte("XMP\x00application/rdf+xml\x00\x00"))
			imageInfo := box("infe", []byte{2, 0, 0, 0, 0, 2, 0, 0}, []byte("av01"), []byte("image\x00"))
			iinf := box("iinf", []byte{0, 0, 0, 0, 0, 2}, infe, imageInfo)
			ftyp := box("ftyp", []byte(brand+"\x00\x00\x00\x00mif1"))
			makeLoc := func(start int) []byte {
				v := []byte{0, 0, 0, 0, 0x44, 0, 0, 2}
				for i, pair := range [][2]int{{start, len(xmp)}, {start + len(xmp), len(pixels)}} {
					v = binary.BigEndian.AppendUint16(v, uint16(i+1))
					v = append(v, 0, 0, 0, 1)
					v = binary.BigEndian.AppendUint32(v, uint32(pair[0]))
					v = binary.BigEndian.AppendUint32(v, uint32(pair[1]))
				}
				return box("iloc", v)
			}
			meta := box("meta", make([]byte, 4), iinf, makeLoc(0))
			meta = box("meta", make([]byte, 4), iinf, makeLoc(len(ftyp)+len(meta)+8))
			data := append(append(ftyp, meta...), box("mdat", xmp, pixels)...)
			clean, err := SanitizeBytes("image."+brand, data)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) != len(clean) || !bytes.HasSuffix(clean, pixels) || !bytes.Contains(clean, meta[:len(meta)-len(makeLoc(0))]) || bytes.Contains(clean, []byte("31,23N")) || !bytes.Contains(clean, []byte(`xml:lang="zh-CN"`)) {
				t.Fatal("HEIF image extents/language changed or GPS remained")
			}
		})
	}
}

func TestMP3LanguageAndSamples(t *testing.T) {
	frame := func(id string, payload []byte) []byte {
		v := append([]byte(id), make([]byte, 6)...)
		binary.BigEndian.PutUint32(v[4:], uint32(len(payload)))
		return append(v, payload...)
	}
	metadata := append(frame("TXXX", []byte("\x03GPSLatitude\x0031,23N")), frame("TLAN", []byte("\x03zho"))...)
	metadata = append(metadata, frame("TIT2", []byte("\x03Readable title"))...)
	header := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, byte(len(metadata) >> 7), byte(len(metadata) & 127)}
	audio := []byte{255, 251, 144, 100, 1, 2, 3, 4, 5}
	data := append(append(header, metadata...), audio...)
	clean, err := SanitizeBytes("audio.mp3", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(clean) || !bytes.HasSuffix(clean, audio) || bytes.Contains(clean, []byte("31,23N")) || !bytes.Contains(clean, []byte("TLAN")) || !bytes.Contains(clean, []byte("Readable title")) {
		t.Fatal("MP3 samples or language changed")
	}
}

func TestFLACLanguageAndSamples(t *testing.T) {
	var comments bytes.Buffer
	str := func(s string) {
		_ = binary.Write(&comments, binary.LittleEndian, uint32(len(s)))
		comments.WriteString(s)
	}
	str("encoder")
	_ = binary.Write(&comments, binary.LittleEndian, uint32(3))
	str("LOCATION=Shanghai")
	str("LANGUAGE=zh-CN")
	str("TITLE=Readable title")
	b := comments.Bytes()
	data := append([]byte("fLaC\x00\x00\x00\x22"), make([]byte, 34)...)
	data = append(data, 0x84, byte(len(b)>>16), byte(len(b)>>8), byte(len(b)))
	data = append(data, b...)
	audio := []byte{255, 248, 1, 2, 3, 4}
	data = append(data, audio...)
	clean, err := SanitizeBytes("audio.flac", data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(clean, audio) || bytes.Contains(clean, []byte("Shanghai")) || !bytes.Contains(clean, []byte("LANGUAGE=zh-CN")) || !bytes.Contains(clean, []byte("Readable title")) {
		t.Fatal("FLAC samples or language changed")
	}
}

func TestExplicitOriginalPolicy(t *testing.T) {
	for name, data := range map[string][]byte{"clip.mkv": []byte("unknown media"), "broken.pdf": []byte("%PDF-1.7 broken"), "broken.jpg": []byte("jpeg bytes")} {
		if p, err := Prepare(name, bytes.NewReader(data), int64(len(data))); err == nil || p != nil {
			t.Fatal("original allowed without consent", name)
		}
		p, err := Prepare(name, bytes.NewReader(data), int64(len(data)), Options{AllowUnclean: true})
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(p.Body)
		_ = p.Close()
		if err != nil || !p.Unclean || !bytes.Equal(out, data) {
			t.Fatal("original bytes or warning lost", name)
		}
	}
	data := pngWithMetadata(t)
	clean, err := SanitizeBytes("photo.png", data, Options{AllowUnclean: true})
	if err != nil || bytes.Contains(clean, []byte("GPSLatitude")) {
		t.Fatal("consent bypassed supported cleaning")
	}
	if !strings.Contains(ErrUnsupported.Error(), "原样") || !strings.Contains(ErrUnsupported.Error(), "元数据") {
		t.Fatal("missing consent warning")
	}
}

func TestOfficePartialConsentWarning(t *testing.T) {
	data := makeZip(t, map[string]string{
		"word/document.xml":        "readable content",
		"docProps/core.xml":        `<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"><cp:keywords>Country=CN</cp:keywords></cp:coreProperties>`,
		"word/media/photo.png":     string(pngWithMetadata(t)),
		"word/embeddings/clip.mkv": "original private container",
	})
	p, err := Prepare("document.docx", bytes.NewReader(data), int64(len(data)), Options{AllowUnclean: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	out, err := io.ReadAll(p.Body)
	if err != nil {
		t.Fatal(err)
	}
	entries := readZipEntries(t, out)
	if !p.Unclean || string(entries["word/embeddings/clip.mkv"]) != "original private container" || bytes.Contains(entries["word/media/photo.png"], []byte("GPSLatitude")) || bytes.Contains(entries["docProps/core.xml"], []byte("Country=CN")) {
		t.Fatal("partial cleanup or original warning lost")
	}
}
