package transfermeta

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"strings"
)

// The workbench accepts at most 2 GB per file. The same cap applies while the
// metadata scrubber stages a recognized file for format-aware rewriting.
const MaxBytes int64 = 2 << 30

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

type format int

const (
	formatUnknown format = iota
	formatJPEG
	formatPNG
	formatGIF
	formatWebP
	formatOOXML
	formatODF
	formatWAV
	formatPDF
	formatTIFF
	formatFLAC
	formatMP3
	formatBMFF
	formatBlocked
)

// Options controls an explicitly accepted original-file fallback.
type Options struct {
	AllowUnclean bool // User explicitly accepted that original metadata may remain.
	warning      *bool
}

func optionsValue(options []Options) Options {
	if len(options) > 0 {
		return options[0]
	}
	return Options{}
}

func (o Options) markUnclean() {
	if o.warning != nil {
		*o.warning = true
	}
}

// Prepared is a host-side body. Unclean reports that any part of the body was
// passed through with explicit consent and may retain metadata.
type Prepared struct {
	Unclean bool
	Body    io.Reader
	Size    int64
	cleanup func() error
}

func (p *Prepared) Close() error {
	if p == nil || p.cleanup == nil {
		return nil
	}
	return p.cleanup()
}

// Prepare inspects the stream and removes supported metadata before it reaches
// the guest. Unsupported media/containers and unsafe rewrites require explicit
// original-file consent. Ordinary non-media files pass through. The size hint
// may be -1 when the caller does not know the content length.
func Prepare(name string, src io.Reader, hint int64, options ...Options) (*Prepared, error) {
	opts := optionsValue(options)
	if opts.warning == nil {
		opts.warning = new(bool)
	}
	if src == nil {
		return nil, errors.New("文件内容为空")
	}
	reader := bufio.NewReaderSize(src, 64<<10)
	head, err := reader.Peek(1024)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return nil, err
	}
	kind := detectFormat(name, head)
	if kind == formatBlocked {
		if opts.AllowUnclean {
			opts.markUnclean()
			return &Prepared{Body: reader, Size: hint, Unclean: true}, nil
		}
		return nil, ErrUnsupported
	}
	if kind == formatUnknown {
		return &Prepared{Unclean: *opts.warning, Body: reader, Size: hint}, nil
	}
	source, size, err := stageSource(reader)
	if err != nil {
		return nil, err
	}
	out, err := os.CreateTemp("", "cage-transfer-output-*")
	if err != nil {
		_ = closeAndRemove(source)
		return nil, err
	}
	changed, err := sanitize(kind, name, source, out, size, opts)
	if err != nil {
		_ = closeAndRemove(out)
		if opts.AllowUnclean {
			opts.markUnclean()
			if _, seekErr := source.Seek(0, 0); seekErr != nil {
				_ = closeAndRemove(source)
				return nil, seekErr
			}
			return &Prepared{Unclean: *opts.warning, Body: source, Size: size, cleanup: func() error { return closeAndRemove(source) }}, nil
		}
		_ = closeAndRemove(source)
		return nil, errors.Join(ErrMalformed, err)
	}
	if !changed {
		_ = out.Close()
		_ = os.Remove(out.Name())
		_, _ = source.Seek(0, 0)
		return &Prepared{Unclean: *opts.warning, Body: source, Size: size, cleanup: func() error { return closeAndRemove(source) }}, nil
	}
	if err := out.Sync(); err != nil {
		_ = closeAndRemove(source)
		_ = closeAndRemove(out)
		return nil, err
	}
	if _, err := out.Seek(0, 0); err != nil {
		_ = closeAndRemove(source)
		_ = closeAndRemove(out)
		return nil, err
	}
	info, err := out.Stat()
	if err != nil {
		_ = closeAndRemove(source)
		_ = closeAndRemove(out)
		return nil, err
	}
	_ = closeAndRemove(source)
	return &Prepared{Unclean: *opts.warning, Body: out, Size: info.Size(), cleanup: func() error { return closeAndRemove(out) }}, nil
}

// SanitizeBytes applies the same format-aware scrub to an in-memory image or
// document. It returns the original slice when no metadata was found.
func SanitizeBytes(name string, data []byte, options ...Options) ([]byte, error) {
	kind := detectFormat(name, data)
	if kind == formatBlocked {
		if optionsValue(options).AllowUnclean {
			optionsValue(options).markUnclean()
			return data, nil
		}
		return nil, ErrUnsupported
	}
	if kind == formatUnknown {
		return data, nil
	}
	var out bytes.Buffer
	changed, err := sanitizeBytes(kind, name, data, &out, optionsValue(options))
	if err != nil {
		if optionsValue(options).AllowUnclean {
			optionsValue(options).markUnclean()
			return data, nil
		}
		return nil, errors.Join(ErrMalformed, err)
	}
	if !changed {
		return data, nil
	}
	return out.Bytes(), nil
}

func detectFormat(name string, head []byte) format {
	if len(head) == 0 {
		return formatUnknown // an empty file carries no metadata
	}
	if len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF {
		return formatJPEG
	}
	if bytes.HasPrefix(head, pngSignature) {
		return formatPNG
	}
	if bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a")) {
		return formatGIF
	}
	if len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		return formatWebP
	}
	if len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE" {
		return formatWAV
	}
	if bytes.HasPrefix(bytes.TrimSpace(head), []byte("%PDF-")) {
		return formatPDF
	}
	if len(head) >= 4 && (bytes.Equal(head[:4], []byte{'I', 'I', 42, 0}) || bytes.Equal(head[:4], []byte{'M', 'M', 0, 42})) {
		return formatTIFF
	}
	if bytes.HasPrefix(head, []byte("fLaC")) {
		return formatFLAC
	}
	if id3v2Header(head) {
		return formatMP3
	}
	if len(head) >= 8 {
		switch string(head[4:8]) {
		case "ftyp", "moov", "wide":
			return formatBMFF
		}
	}
	if blockedFormat(name, head) {
		return formatBlocked
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".docx", ".docm", ".xlsx", ".xlsm", ".pptx", ".pptm":
		return formatOOXML
	case ".odt", ".ods", ".odp", ".odg", ".odf":
		return formatODF
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".wav":
		return formatBlocked // a supported extension with an invalid signature
	}
	return formatUnknown
}

func stageSource(src io.Reader) (*os.File, int64, error) {
	file, err := os.CreateTemp("", "cage-transfer-source-*")
	if err != nil {
		return nil, 0, err
	}
	count, err := io.Copy(file, io.LimitReader(src, MaxBytes+1))
	if err != nil {
		_ = closeAndRemove(file)
		return nil, 0, err
	}
	if count > MaxBytes {
		_ = closeAndRemove(file)
		return nil, 0, errors.New("每个文件不能超过 2 GB")
	}
	if err := file.Sync(); err != nil {
		_ = closeAndRemove(file)
		return nil, 0, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = closeAndRemove(file)
		return nil, 0, err
	}
	return file, count, nil
}

func closeAndRemove(file *os.File) error {
	if file == nil {
		return nil
	}
	name := file.Name()
	err := file.Close()
	if removeErr := os.Remove(name); removeErr != nil && !os.IsNotExist(removeErr) {
		err = errors.Join(err, removeErr)
	}
	return err
}

func sanitize(kind format, name string, src *os.File, dst io.Writer, size int64, options Options) (bool, error) {
	switch kind {
	case formatJPEG:
		return sanitizeJPEG(src, dst)
	case formatPNG:
		return sanitizePNG(src, dst)
	case formatGIF:
		return sanitizeGIF(src, dst)
	case formatWebP:
		return sanitizeWebP(src, dst, size)
	case formatOOXML, formatODF:
		return sanitizeZipDoc(name, src, dst, size, kind, options)
	case formatPDF:
		return sanitizePDF(src, dst)
	case formatTIFF:
		return sanitizeTIFF(src, dst, size)
	case formatFLAC:
		return sanitizeFLAC(src, dst)
	case formatMP3:
		return sanitizeMP3(src, dst, size)
	case formatBMFF:
		return sanitizeBMFF(src, dst, size)
	case formatWAV:
		return sanitizeWAV(src, dst, size)
	}
	return false, nil
}

func sanitizeBytes(kind format, name string, data []byte, dst io.Writer, options Options) (bool, error) {
	switch kind {
	case formatJPEG:
		return sanitizeJPEG(bytes.NewReader(data), dst)
	case formatPNG:
		return sanitizePNG(bytes.NewReader(data), dst)
	case formatGIF:
		return sanitizeGIF(bytes.NewReader(data), dst)
	case formatWebP:
		return sanitizeWebP(bytes.NewReader(data), dst, int64(len(data)))
	case formatOOXML, formatODF:
		return sanitizeZipBytes(name, data, dst, kind, options)
	case formatPDF:
		return sanitizePDF(bytes.NewReader(data), dst)
	case formatTIFF:
		return sanitizeTIFF(bytes.NewReader(data), dst, int64(len(data)))
	case formatFLAC:
		return sanitizeFLAC(bytes.NewReader(data), dst)
	case formatMP3:
		return sanitizeMP3(bytes.NewReader(data), dst, int64(len(data)))
	case formatBMFF:
		return sanitizeBMFF(bytes.NewReader(data), dst, int64(len(data)))
	case formatWAV:
		return sanitizeWAV(bytes.NewReader(data), dst, int64(len(data)))
	}
	return false, nil
}
