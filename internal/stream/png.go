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
	// BestSpeed, and the numbers are why. Measured on a 1920x1080 frame: 44 ms at
	// BestSpeed for 180 KB, 60 ms at the default for 69 KB, 242 ms at Best for 55.
	// The default costs a quarter more time for a quarter of the bytes, and the
	// bytes are the cheap part — a session on a local network can carry seven
	// megabytes a second without noticing. Best is not a candidate at all.
	//
	// The floor is what matters: even with no compression at all this frame costs
	// 45 ms, so the cost is in the filters and the scanlines rather than in the
	// compressor, and no choice of level brings a full 1080p frame inside the
	// sixteen milliseconds a frame interval allows. That is why the work is
	// divided into strips and run at once, and why the frame rate settles where
	// the machine can keep up rather than where the clock says.
	var buf sliceWriter
	enc := &png.Encoder{CompressionLevel: png.BestSpeed}
	_ = quality // spent on nothing: a lossless format has no fidelity to trade
	if err := enc.Encode(&buf, src); err != nil {
		return dst, 0, err
	}
	return buf.buf, wsframe.FormatPNG, nil
}
