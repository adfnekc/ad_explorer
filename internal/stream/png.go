package stream

import (
	"image"
	"image/png"

	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// PNG encodes with the standard library, losslessly.
//
// Lossless is the reason for it, and it is not a small one. This is a desktop:
// text, window frames, flat colour, a caret. JPEG smears exactly those things at
// any quality worth sending, so the picture arrives, is not wrong, and cannot be
// read. Measured on a synthetic desktop, a 1280x720 frame is 36 KB as PNG
// against 405 KB as JPEG at quality 85, and a 400x250 region is 7 KB against
// 45 KB — so the sharp picture is also the smaller one, and the choice is not
// the usual trade.
//
// It is slower, and by less than that sounds: 25 ms against 19 for a full frame
// and 3.4 against 2.4 for a region. What that buys is that a session which is
// behind can drop a frame, while a session that is blurry has nothing to drop.
//
// Photographic content is where this loses. A full-screen wallpaper is a large
// PNG and a small JPEG, and a session that spends its time there will send more
// bytes than it used to. That is a real cost, and it is the one an operator
// should be able to overrule, which is why the encoder is a setting rather than
// a constant.
type PNG struct{}

// Name implements ImageEncoder.
func (PNG) Name() string { return "png" }

// Encode implements ImageEncoder.
func (PNG) Encode(dst []byte, src image.Image, quality int) ([]byte, uint8, error) {
	if src == nil {
		return dst, 0, ErrEmptyRegion
	}
	if src.Bounds().Empty() {
		return dst, 0, ErrEmptyRegion
	}

	// The caller's dst is built up separately: the encoder needs to write a
	// whole stream rather than append, so the slice it is handed has to be the
	// start of one. dst is therefore left untouched and reusable, which is the
	// promise the JPEG encoder makes as well.
	var buf sliceWriter
	enc := &png.Encoder{CompressionLevel: png.BestSpeed}
	// The caller's quality is spent on effort rather than fidelity, because
	// lossless has no fidelity to trade. A desktop that redraws the same region
	// repeatedly is worth compressing harder; one that is changing is not.
	if quality >= 60 {
		enc.CompressionLevel = png.DefaultCompression
	}
	if err := enc.Encode(&buf, src); err != nil {
		return dst, 0, err
	}
	return buf.buf, wsframe.FormatPNG, nil
}
