package transfermeta

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"time"
)

var fixedZipTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

func sanitizeZipDoc(name string, src *os.File, dst io.Writer, size int64, kind format, options Options) (bool, error) {
	reader, err := zip.NewReader(src, size)
	if err != nil {
		return false, ErrMalformed
	}
	return sanitizeZipArchive(reader, dst, kind, options)
}

func sanitizeZipBytes(name string, data []byte, dst io.Writer, kind format, options Options) (bool, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false, ErrMalformed
	}
	return sanitizeZipArchive(reader, dst, kind, options)
}

func sanitizeZipArchive(reader *zip.Reader, dst io.Writer, kind format, options Options) (bool, error) {
	if len(reader.File) > 4096 {
		return false, ErrUnsupported
	}
	writer := zip.NewWriter(dst)
	var total uint64
	for _, entry := range reader.File {
		if entry.UncompressedSize64 > 64<<20 || total > 256<<20-entry.UncompressedSize64 {
			return false, ErrUnsupported
		}
		total += entry.UncompressedSize64
		data, err := readZipEntry(entry)
		if err != nil {
			return false, err
		}
		content := data
		switch {
		case kind == formatOOXML && ooxmlMetadataName(entry.Name):
			if local := ooxmlMetadataRoot(entry.Name); local != "" {
				if cleaned, ok := clearXMLRoot(data, local); ok {
					content = cleaned
				} else {
					return false, ErrMalformed
				}
			}
		case kind == formatODF && entry.Name == "meta.xml":
			if cleaned, ok := clearXMLRoot(data, "document-meta"); ok {
				content = cleaned
			} else {
				return false, ErrMalformed
			}
			// settings.xml is functional content and is preserved.
		}
		// Nested document containers need a separate recursive resource budget;
		// deny them rather than allowing arbitrarily deep decompression.
		nested := detectFormat(entry.Name, content)
		if nested == formatOOXML || nested == formatODF {
			if !options.AllowUnclean {
				return false, ErrUnsupported
			}
			options.markUnclean()
			nested = formatUnknown
		}
		var cleaned []byte
		if nested == formatUnknown {
			cleaned = content
		} else {
			cleaned, err = SanitizeBytes(entry.Name, content, options)
		}
		if err != nil {
			return false, err
		}
		content = cleaned
		header := entry.FileHeader
		header.Modified = fixedZipTime
		header.Extra = nil
		header.Comment = ""
		header.ModifiedDate = 33
		header.ModifiedTime = 0
		header.CRC32 = 0
		header.CompressedSize = 0
		header.UncompressedSize = 0
		header.CompressedSize64 = 0
		header.UncompressedSize64 = 0
		part, err := writer.CreateHeader(&header)
		if err != nil {
			return false, err
		}
		if _, err := part.Write(content); err != nil {
			return false, err
		}
	}
	if err := writer.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func readZipEntry(entry *zip.File) ([]byte, error) {
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, 64<<20+1))
}

func ooxmlMetadataName(name string) bool {
	switch name {
	case "docProps/core.xml", "docProps/app.xml", "docProps/custom.xml":
		return true
	}
	return false
}

func ooxmlMetadataRoot(name string) string {
	switch name {
	case "docProps/core.xml":
		return "coreProperties"
	case "docProps/app.xml", "docProps/custom.xml":
		return "Properties"
	}
	return ""
}

func clearXMLRoot(data []byte, localName string) ([]byte, bool) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth, roots := 0, 0
	var root xml.Name
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		switch t := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				root = t.Name
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) != 0 {
				return nil, false
			}
		}
	}
	if roots != 1 || depth != 0 || root.Local != localName {
		return nil, false
	}
	// Fixed namespaces avoid retaining xml:lang, custom root attributes or
	// private namespace declarations from the original property document.
	namespace := root.Space
	switch localName {
	case "coreProperties":
		namespace = "http://schemas.openxmlformats.org/package/2006/metadata/core-properties"
	case "Properties":
		if namespace != "http://schemas.openxmlformats.org/officeDocument/2006/custom-properties" {
			namespace = "http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"
		}
	case "document-meta", "document-settings":
		namespace = "urn:oasis:names:tc:opendocument:xmlns:office:1.0"
	default:
		return nil, false
	}
	start := xml.StartElement{Name: xml.Name{Space: namespace, Local: localName}}
	var out bytes.Buffer
	encoder := xml.NewEncoder(&out)
	if encoder.EncodeToken(start) != nil {
		return nil, false
	}
	if localName == "document-meta" {
		meta := xml.StartElement{Name: xml.Name{Space: namespace, Local: "meta"}}
		if encoder.EncodeToken(meta) != nil || encoder.EncodeToken(meta.End()) != nil {
			return nil, false
		}
	}
	if encoder.EncodeToken(start.End()) != nil || encoder.Flush() != nil {
		return nil, false
	}
	return out.Bytes(), true
}
