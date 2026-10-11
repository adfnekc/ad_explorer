package stream

import (
	"errors"
	"image"
	"image/color"
	"image/jpeg"

	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// ErrEmptyRegion means a region with no pixels reached the encoder. It is a
// bug in the caller rather than a condition to recover from, so it is reported
// instead of producing a zero-byte frame the client would draw as nothing.
var ErrEmptyRegion = errors.New("stream: region is empty")

// ImageEncoder encodes one region of a desktop.
//
// It is an interface so the format is a choice made in one place: JPEG comes
// from the standard library and needs no cgo, and a WebP encoder can be dropped
// in later without touching the frame builder.
type ImageEncoder interface {
	// Encode writes the encoding of src to the end of dst and reports the
	// format byte to put in the wire header. On error, dst is unchanged.
	Encode(dst []byte, src image.Image, quality int) ([]byte, uint8, error)
	// Name is for logs and stats.
	Name() string
}

// JPEG encodes with the standard library.
//
// It stays available because it is the right answer for photographic content,
// where PNG's losslessness costs far more bytes than the detail is worth. It is
// no longer the default: see PNG for the measurement that moved it.
type JPEG struct{}

// Name implements ImageEncoder.
func (JPEG) Name() string { return "jpeg" }

// Encode implements ImageEncoder.
func (JPEG) Encode(dst []byte, src image.Image, quality int) ([]byte, uint8, error) {
	if src == nil {
		return dst, 0, ErrEmptyRegion
	}
	if src.Bounds().Empty() {
		return dst, 0, ErrEmptyRegion
	}

	w := &sliceWriter{buf: dst}
	if err := jpeg.Encode(w, src, &jpeg.Options{Quality: clampQuality(quality)}); err != nil {
		// The writer appended into a possibly reallocated slice, so the
		// caller's dst is untouched and can be reused.
		return dst, 0, err
	}
	return w.buf, wsframe.FormatJPEG, nil
}

// sliceWriter appends to a slice, so encoding goes straight into the caller's
// output buffer instead of through an intermediate one that has to be copied.
type sliceWriter struct{ buf []byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// clampQuality keeps a quality inside what the JPEG encoder accepts. The
// encoder treats 0 as "use the default", so a caller asking for 0 would
// silently get 75, which is the kind of surprise worth preventing.
func clampQuality(q int) int {
	if q < 1 {
		return 1
	}
	if q > 100 {
		return 100
	}
	return q
}

// Builder turns a framebuffer and a dirty set into frames ready to write.
//
// It is not safe for concurrent use; the caller owns it.
type Builder struct {
	enc ImageEncoder
	seq uint32
}

// NewBuilder returns a builder using enc. A nil encoder gets JPEG.
func NewBuilder(enc ImageEncoder) *Builder {
	if enc == nil {
		enc = PNG{}
	}
	return &Builder{enc: enc}
}

// EncoderName reports which encoder is in use.
func (b *Builder) EncoderName() string { return b.enc.Name() }

// Build encodes every region the accumulator holds against src, which is the
// whole desktop.
//
// The payloads are freshly allocated, so the caller may reuse or overwrite the
// framebuffer as soon as this returns. Sequence numbers advance once per frame
// and never reset, which is what lets the client notice a gap.
func (b *Builder) Build(src image.Image, acc *Accumulator, quality int) ([]wsframe.Frame, error) {
	if src == nil {
		return nil, ErrEmptyRegion
	}
	rects, full := acc.Result()
	if full {
		rects = []image.Rectangle{acc.Bounds()}
	}
	if len(rects) == 0 {
		return nil, nil
	}

	bounds := acc.Bounds()
	frames := make([]wsframe.Frame, 0, len(rects))
	for _, r := range rects {
		// The rectangle is clipped here, before the pixels are taken, so that
		// the header and the payload describe the same thing. The client scales
		// a region to the rectangle it is given, so a header that says more than
		// the payload holds stretches the pixels across the difference, and the
		// area it stretches into stays wrong until something else repaints it.
		// Clipping inside crop alone was enough to leave the header saying one
		// thing and the payload another.
		r = r.Intersect(bounds)
		if r.Empty() {
			// No pixels to draw: skipping it cannot lose any, whereas letting it
			// reach the encoder fails the whole frame and takes the regions that
			// were already built with it.
			continue
		}
		region, err := crop(src, r, bounds)
		if err != nil {
			return nil, err
		}
		payload, format, err := b.enc.Encode(nil, region, quality)
		if err != nil {
			return nil, err
		}
		b.seq++

		var flags uint16
		if full {
			flags |= wsframe.FlagFull
		}
		if b.seq == 1 {
			flags |= wsframe.FlagKeyframe
		}
		frames = append(frames, wsframe.Frame{
			Type:    wsframe.TypeImage,
			Format:  format,
			Flags:   flags,
			Rect:    r,
			Seq:     b.seq,
			Payload: payload,
		})
	}
	return frames, nil
}

// crop returns the part of src covering r.
//
// It uses SubImage when the source offers it, which shares the pixels instead
// of copying them, and falls back to translating the bounds so that an
// image.Image that only implements the basic interface still works. The bounds
// are clamped to the desktop because the wire format can describe a region
// larger than the screen and the encoder cannot read past the buffer.
func crop(src image.Image, r, desktop image.Rectangle) (image.Image, error) {
	r = r.Intersect(desktop)
	if r.Empty() {
		return nil, ErrEmptyRegion
	}
	if sub, ok := src.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return sub.SubImage(r), nil
	}
	if src.Bounds() == r {
		return src, nil
	}
	// No SubImage: present the region as a view with its own origin so the
	// encoder reads the right pixels.
	return &offsetImage{src: src, offset: r.Min, size: r.Size()}, nil
}

// offsetImage presents a window onto another image.
type offsetImage struct {
	src    image.Image
	offset image.Point
	size   image.Point
}

func (o *offsetImage) ColorModel() color.Model { return o.src.ColorModel() }
func (o *offsetImage) Bounds() image.Rectangle { return image.Rectangle{Max: o.size} }
func (o *offsetImage) At(x, y int) color.Color {
	return o.src.At(x+o.offset.X, y+o.offset.Y)
}
