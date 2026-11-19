package transfermeta

import (
	"archive/tar"
	"errors"
	"io"
	"os"
)

// SanitizeArchive cleans each clipboard file, including files in directories.
// Rebuilding headers also drops PAX/xattr records, owner names and timestamps.
// An error never copies the offending original file to the destination.
func SanitizeArchive(dst io.Writer, src io.Reader, options ...Options) error {
	r := tar.NewReader(src)
	w := tar.NewWriter(dst)
	var total int64
	for {
		h, err := r.Next()
		if err == io.EOF {
			return w.Close()
		}
		if err != nil {
			return err
		}
		if h.Size < 0 || h.Size > MaxBytes || total > (200<<30)-h.Size {
			return ErrUnsupported
		}
		total += h.Size
		header := &tar.Header{Name: h.Name, Mode: h.Mode & 0777, Typeflag: h.Typeflag, Format: tar.FormatPAX}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := w.WriteHeader(header); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			p, err := Prepare(h.Name, r, h.Size, optionsValue(options))
			if err != nil {
				return err
			}
			header.Size = p.Size
			err = w.WriteHeader(header)
			if err == nil {
				_, err = io.CopyN(w, p.Body, p.Size)
			}
			err = errors.Join(err, p.Close())
			if err != nil {
				return err
			}
		default:
			return errors.New("剪贴板不支持链接或特殊文件")
		}
	}
}

// PrepareArchive completes validation on the host before a guest import starts.
func PrepareArchive(src io.Reader, options ...Options) (*Prepared, error) {
	opts := optionsValue(options)
	if opts.warning == nil {
		opts.warning = new(bool)
	}
	out, err := os.CreateTemp("", "cage-transfer-archive-*")
	if err != nil {
		return nil, err
	}
	if err = SanitizeArchive(out, src, opts); err != nil {
		_ = closeAndRemove(out)
		return nil, err
	}
	size, err := out.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = out.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = closeAndRemove(out)
		return nil, err
	}
	return &Prepared{Unclean: *opts.warning, Body: out, Size: size, cleanup: func() error { return closeAndRemove(out) }}, nil
}
