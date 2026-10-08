package stream

import (
	"image"
	"image/color"
	"testing"

	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// makeDesktop builds a frame that is not one flat colour, so the codecs do the work
// a real screen makes them do rather than taking a shortcut through a uniform
// block.
func makeDesktop(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x*7 + y*3) % 256),
				G: uint8((x*3 + y*11) % 256),
				B: uint8((x ^ y) % 256),
				A: 255,
			})
		}
	}
	return img
}

// BenchmarkJPEGFullFrame is the worst case: a whole 1280x720 desktop re-encoded
// because enough of it changed to make a full repaint cheaper than the parts.
func BenchmarkJPEGFullFrame(b *testing.B) {
	img := makeDesktop(1280, 720)
	b.ReportAllocs()
	b.SetBytes(int64(len(img.Pix)))

	for i := 0; i < b.N; i++ {
		if _, _, err := (JPEG{}).Encode(nil, img, 70); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJPEGRegion is the common case: a window, a caret, a clock.
func BenchmarkJPEGRegion(b *testing.B) {
	full := makeDesktop(1280, 720)
	region := full.SubImage(image.Rect(200, 150, 600, 400)).(*image.RGBA)
	b.ReportAllocs()
	b.SetBytes(int64(len(region.Pix)))

	for i := 0; i < b.N; i++ {
		if _, _, err := (JPEG{}).Encode(nil, region, 70); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuildFullFrame measures what the session actually calls: crop,
// encode, and build the frames for the wire.
func BenchmarkBuildFullFrame(b *testing.B) {
	img := makeDesktop(1280, 720)
	builder := NewBuilder(JPEG{})
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		acc := NewAccumulator(img.Bounds(), DefaultMaxRects, DefaultFullFraction)
		acc.Add(img.Bounds())
		if _, err := builder.Build(img, acc, 70); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAccumulatorManyRegions is the merging cost when a busy desktop
// reports a lot of small changes in one batch.
func BenchmarkAccumulatorManyRegions(b *testing.B) {
	bounds := image.Rect(0, 0, 1280, 720)
	regions := make([]image.Rectangle, 0, 64)
	for i := 0; i < 64; i++ {
		x := (i * 97) % 1200
		y := (i * 53) % 640
		regions = append(regions, image.Rect(x, y, x+40, y+30))
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		acc := NewAccumulator(bounds, DefaultMaxRects, DefaultFullFraction)
		for _, r := range regions {
			acc.Add(r)
		}
		acc.Result()
	}
}

// BenchmarkFrameHeader measures the per-frame wire cost, which is what a client
// pays per region on top of the image itself.
func BenchmarkFrameHeader(b *testing.B) {
	f := Frame{
		Type: wsframe.TypeImage, Format: wsframe.FormatJPEG,
		Rect: image.Rect(0, 0, 400, 250), Payload: make([]byte, 12<<10),
	}
	buf := make([]byte, 0, 64<<10)
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		var err error
		buf, err = f.AppendTo(buf[:0])
		if err != nil {
			b.Fatal(err)
		}
	}
}

// Frame is a local alias so the benchmark reads clearly.
type Frame = wsframe.Frame
