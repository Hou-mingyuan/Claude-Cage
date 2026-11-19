package transfermeta

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// Use a PDF object parser, never regex over compressed/encrypted page streams.
// Signed/encrypted PDFs use the explicit-original path: any rewrite can change
// signature validity or access semantics. Catalog /Lang and font CMaps stay.
func sanitizePDF(src io.ReadSeeker, dst io.Writer) (changed bool, err error) {
	defer func() {
		if recover() != nil {
			changed = false
			err = ErrUnsupported
		}
	}()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conf := model.NewStatelessConfiguration()
	conf.Offline = true
	conf.ValidateLinks = false
	conf.Optimize = false
	conf.OptimizeBeforeWriting = false
	conf.PreserveInfoDict = true
	conf.WriteObjectStream = false
	conf.WriteXRefStream = false
	conf.Limits.MaxInputBytes = 256 << 20
	conf.Limits.MaxDecodeBytes = 64 << 20
	conf.Limits.MaxStreamBytes = 64 << 20
	conf.Limits.MaxObjectBytes = 64 << 20
	ctx, err := api.ReadContext(c, src, conf)
	if err != nil {
		return false, ErrUnsupported
	}
	if err = api.ValidateContext(c, ctx); err != nil {
		return false, ErrUnsupported
	}
	if ctx.Encrypt != nil || ctx.SignatureExist || len(ctx.Signatures) > 0 {
		return false, ErrUnsupported
	}
	// Materialize object streams as well as directly stored objects. The parser
	// enforces its resource limits while dereferencing and decoding metadata.
	for n, e := range ctx.Table {
		if e.Free {
			continue
		}
		gen := 0
		if e.Generation != nil {
			gen = *e.Generation
		}
		o, er := ctx.Dereference(*types.NewIndirectRef(n, gen))
		if er != nil {
			return false, ErrUnsupported
		}
		switch v := o.(type) {
		case types.Dict:
			if v.NameEntry("Type") != nil && *v.NameEntry("Type") == "Sig" || v["ByteRange"] != nil {
				return false, ErrUnsupported
			}
		case types.StreamDict:
			if v.Type() == nil || *v.Type() != "Metadata" {
				continue
			}
			if er := ctx.DecodeStream(&v); er != nil {
				return false, ErrUnsupported
			}
			clean, modified, er := scrubRegionalXML(v.Content)
			if er != nil {
				return false, er
			}
			if modified {
				v.Content = clean
				if er := v.Encode(); er != nil {
					return false, ErrUnsupported
				}
				e.Object = v
				changed = true
			}
		}
	}
	if ctx.Info != nil {
		d, er := ctx.DereferenceDict(*ctx.Info)
		if er != nil {
			return false, ErrUnsupported
		}
		for k, v := range d {
			value, er := ctx.Dereference(v)
			if er != nil {
				return false, ErrUnsupported
			}
			text, _ := types.StringOrHexLiteral(value)
			if regionalKey(k) || text != nil && (regionalDate(k, *text) || strings.EqualFold(k, "Language") && regionLanguage.MatchString(*text)) {
				delete(d, k)
				changed = true
			}
		}
	} else if changed {
		// Prevent the writer from introducing the host's current local offset.
		ctx.Info, err = ctx.IndRefForNewObject(types.NewDict())
		if err != nil {
			return false, err
		}
	}
	if !changed {
		return false, nil
	}
	if err = api.WriteContext(c, ctx, dst); err != nil {
		return false, ErrUnsupported
	}
	return true, nil
}
