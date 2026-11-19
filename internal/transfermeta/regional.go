package transfermeta

import (
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Language/locale, character encodings, fonts and subtitle language are not
// provenance by themselves. Do not remove them from functional structures.
func regionalKey(key string) bool {
	// Compare complete field names, never arbitrary substrings: "opacity"
	// contains "city", but can control rendering and must be preserved.
	k := strings.ToLower(strings.TrimSpace(key))
	parts := strings.FieldsFunc(k, func(r rune) bool { return r == '.' || r == ':' || r == '/' })
	if len(parts) == 0 {
		return false
	}
	compact := strings.NewReplacer("_", "", "-", "", " ", "").Replace(parts[len(parts)-1])
	if strings.HasPrefix(compact, "gps") {
		return true
	}
	switch compact {
	case "latitude", "longitude", "location", "locationiso6709", "locationinformation", "locationname", "locationcreated", "locationshown",
		"country", "countrycode", "countryprimarylocationcode", "primarylocationname", "timezone", "timezoneoffset", "offsettime", "offsettimeoriginal", "offsettimedigitized",
		"region", "regioncode", "state", "province", "city", "sublocation":
		return true
	}
	return len(parts) > 1 && parts[len(parts)-2] == "location" && compact == "iso6709"
}

var timezoneOffset = regexp.MustCompile(`(?:[T ]\d{2}:?\d{2}(?::?\d{2}(?:\.\d+)?)?|D:\d{8,14})\s*[+-]\d{2}(?::?\d{2}|['’]\d{2}['’]?)`)

var regionLanguage = regexp.MustCompile(`(?i)^[a-z]{2,3}(?:-[a-z]{4})?-(?:[a-z]{2}|[0-9]{3})$`)

func regionalDate(key, value string) bool {
	k := strings.ToLower(key)
	return (strings.Contains(k, "date") || strings.Contains(k, "time")) && timezoneOffset.MatchString(value)
}

func provenanceNamespace(space string) bool {
	switch space {
	case "http://ns.adobe.com/exif/1.0/", "http://cipa.jp/exif/1.0/", "http://ns.adobe.com/exif/1.0/aux/",
		"http://ns.adobe.com/photoshop/1.0/", "http://ns.adobe.com/xap/1.0/", "http://purl.org/dc/elements/1.1/",
		"http://iptc.org/std/Iptc4xmpCore/1.0/xmlns/", "http://iptc.org/std/Iptc4xmpExt/2008-02-29/":
		return true
	}
	return false
}

type byteRange struct{ start, end int }

// Edit only optional XMP provenance fields. Keeping byte positions lets TIFF
// and ISO BMFF retain all offsets and image/audio/video bitstreams unchanged.
func scrubRegionalXML(data []byte) ([]byte, bool, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	type element struct {
		start  int
		name   string
		space  string
		remove bool
		text   strings.Builder
	}
	var stack []*element
	var spans []byteRange
	roots := 0
	for tokens := 0; ; tokens++ {
		if tokens > 65536 || len(stack) > 128 {
			return nil, false, ErrUnsupported
		}
		start := int(d.InputOffset())
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, ErrUnsupported
		}
		end := int(d.InputOffset())
		switch t := token.(type) {
		case xml.StartElement:
			if len(stack) == 0 {
				roots++
				if !(t.Name.Local == "xmpmeta" && t.Name.Space == "adobe:ns:meta/" || t.Name.Local == "RDF" && t.Name.Space == "http://www.w3.org/1999/02/22-rdf-syntax-ns#") {
					return nil, false, ErrUnsupported
				}
			}
			if regionalKey(t.Name.Local) && !provenanceNamespace(t.Name.Space) {
				return nil, false, ErrUnsupported
			}
			stack = append(stack, &element{start: start, name: t.Name.Local, space: t.Name.Space, remove: regionalKey(t.Name.Local)})
			attrs, err := xmlAttributeRanges(data[start:end])
			if err != nil || len(attrs) != len(t.Attr) {
				return nil, false, ErrUnsupported
			}
			for i, a := range t.Attr {
				if a.Name.Space == "xmlns" || a.Name.Local == "xmlns" {
					continue
				}
				candidate := regionalKey(a.Name.Local) || regionalDate(a.Name.Local, a.Value)
				if candidate && !provenanceNamespace(a.Name.Space) {
					return nil, false, ErrUnsupported
				}
				if candidate {
					spans = append(spans, byteRange{start + attrs[i].start, start + attrs[i].end})
				}
			}
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			} else if len(bytes.TrimSpace(t)) > 0 {
				return nil, false, ErrUnsupported
			}
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, false, ErrUnsupported
			}
			e := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if len(stack) > 0 && stack[len(stack)-1].name == "language" {
				stack[len(stack)-1].text.WriteString(e.text.String())
			}
			optionalLanguage := e.name == "language" && e.space == "http://purl.org/dc/elements/1.1/" && regionLanguage.MatchString(strings.TrimSpace(e.text.String()))
			dateField := regionalDate(e.name, e.text.String())
			if dateField && !provenanceNamespace(e.space) {
				return nil, false, ErrUnsupported
			}
			if e.remove || dateField || optionalLanguage {
				if len(stack) == 0 {
					return nil, false, ErrUnsupported
				}
				spans = append(spans, byteRange{e.start, end})
			}
		case xml.Comment:
			spans = append(spans, byteRange{start, end})
		}
	}
	if roots != 1 || len(stack) != 0 {
		return nil, false, ErrUnsupported
	}
	if len(spans) == 0 {
		return data, false, nil
	}
	out := bytes.Clone(data)
	for _, span := range spans {
		for i := span.start; i < span.end; i++ {
			out[i] = ' '
		}
	}
	return out, true, nil
}

func xmlAttributeRanges(tag []byte) ([]byteRange, error) {
	i := 1
	for i < len(tag) && !xmlSpace(tag[i]) && tag[i] != '>' && tag[i] != '/' {
		i++
	}
	var out []byteRange
	for i < len(tag) {
		for i < len(tag) && xmlSpace(tag[i]) {
			i++
		}
		if i >= len(tag) || tag[i] == '>' || tag[i] == '/' {
			break
		}
		start := i
		for i < len(tag) && tag[i] != '=' {
			i++
		}
		i++
		for i < len(tag) && xmlSpace(tag[i]) {
			i++
		}
		if i >= len(tag) || tag[i] != '\'' && tag[i] != '"' {
			return nil, ErrUnsupported
		}
		quote := tag[i]
		i++
		for i < len(tag) && tag[i] != quote {
			i++
		}
		if i >= len(tag) {
			return nil, ErrUnsupported
		}
		i++
		out = append(out, byteRange{start, i})
	}
	return out, nil
}

func xmlSpace(b byte) bool { return b == ' ' || b == '\n' || b == '\r' || b == '\t' }

type filePatch struct {
	offset int64
	data   []byte
}

// A patch never shifts bytes. All structural validation finishes before output.
func writePatched(src io.ReadSeeker, dst io.Writer, patches []filePatch) (bool, error) {
	sort.Slice(patches, func(i, j int) bool { return patches[i].offset < patches[j].offset })
	var pos int64
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	for _, p := range patches {
		if p.offset < pos {
			return false, ErrUnsupported
		}
		if _, err := io.CopyN(dst, src, p.offset-pos); err != nil {
			return false, err
		}
		if _, err := dst.Write(p.data); err != nil {
			return false, err
		}
		pos = p.offset + int64(len(p.data))
		if _, err := src.Seek(pos, io.SeekStart); err != nil {
			return false, err
		}
	}
	_, err := io.Copy(dst, src)
	return len(patches) > 0, err
}
