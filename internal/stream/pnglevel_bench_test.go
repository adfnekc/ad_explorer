package stream

import (
	"image"
	"image/png"
	"testing"

	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

type pngAt struct{ level png.CompressionLevel }

func (p pngAt) Name() string { return "png" }

func (p pngAt) Encode(dst []byte, src image.Image, quality int) ([]byte, uint8, error) {
	if src == nil || src.Bounds().Empty() {
		return dst, 0, ErrEmptyRegion
	}
	var buf sliceWriter
	enc := &png.Encoder{CompressionLevel: p.level}
	if err := enc.Encode(&buf, src); err != nil {
		return dst, 0, err
	}
	return buf.buf, wsframe.FormatPNG, nil
}

// The compressor level is the whole of what the quality setting controls for a
// lossless format, and it is a straight trade of CPU for bytes. These are timed
// on the workload that matters: one 1080p frame, which is what the frame loop
// has a 16 ms budget for.
func BenchmarkPNGLevels1080p(b *testing.B) {
	full := makeDesktop(1920, 1080)
	for _, tc := range []struct {
		name  string
		level png.CompressionLevel
	}{
		{"NoCompression", png.NoCompression},
		{"BestSpeed", png.BestSpeed},
		{"Default", png.DefaultCompression},
		{"Best", png.BestCompression},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchEncoder(b, pngAt{tc.level}, full, 85)
		})
	}
}
