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

// BenchmarkPNGFullFrame is the same worst case as above with the codec the
// session actually uses. The two are not interchangeable: PNG has no quality to
// trade and spends its time on per-scanline filtering rather than on a
// compressor that can be told to hurry up, so an encoder benchmark that uses
// JPEG says nothing about what a session pays.
func BenchmarkPNGFullFrame(b *testing.B) {
	img := makeDesktop(1280, 720)
	b.ReportAllocs()
	b.SetBytes(int64(len(img.Pix)))

	for i := 0; i < b.N; i++ {
		if _, _, err := (PNG{}).Encode(nil, img, 70); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPNGRegion is the same common case as above with PNG.
func BenchmarkPNGRegion(b *testing.B) {
	full := makeDesktop(1280, 720)
	region := full.SubImage(image.Rect(200, 150, 600, 400)).(*image.RGBA)
	b.ReportAllocs()
	b.SetBytes(int64(len(region.Pix)))

	for i := 0; i < b.N; i++ {
		if _, _, err := (PNG{}).Encode(nil, region, 70); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuildFullFramePNG is what a session calls when the whole desktop
// changed and the codec is the one it ships with.
func BenchmarkBuildFullFramePNG(b *testing.B) {
	img := makeDesktop(1280, 720)
	builder := NewBuilder(PNG{})
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		acc := NewAccumulator(img.Bounds(), DefaultMaxRects, DefaultFullFraction)
		acc.Add(img.Bounds())
		if _, err := builder.Build(img, acc, 70); err != nil {
			b.Fatal(err)
		}
	}
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

// The encoders side by side, reporting size as well as time. The trade between
// them is sharper against smaller, and which way it falls is a property of what
// is on the screen rather than of the encoders.
func benchEncoder(b *testing.B, enc ImageEncoder, img image.Image, quality int) {
	b.Helper()
	b.ReportAllocs()
	var size int
	for i := 0; i < b.N; i++ {
		out, _, err := enc.Encode(nil, img, quality)
		if err != nil {
			b.Fatal(err)
		}
		size = len(out)
	}
	b.SetBytes(int64(size))
	b.ReportMetric(float64(size), "encBytes")
}

func BenchmarkJPEGFullFrameSized(b *testing.B) {
	benchEncoder(b, JPEG{}, makeDesktop(1280, 720), 85)
}

func BenchmarkJPEGRegionSized(b *testing.B) {
	full := makeDesktop(1280, 720)
	benchEncoder(b, JPEG{}, full.SubImage(image.Rect(200, 150, 600, 400)).(*image.RGBA), 85)
}

// One frame of a scrolling 1080p desktop is many regions, and they are encoded
// one after another today. This measures what that costs, which is the number
// that decides whether the frame loop can keep up with a scroll.
func BenchmarkScrollRegionsSerial(b *testing.B) {
	full := makeDesktop(1920, 1080)
	const regions = 24 // a scrolling window, a taskbar, a clock, a caret, edges
	rects := make([]image.Rectangle, 0, regions)
	for i := 0; i < regions; i++ {
		x := (i % 6) * 320
		y := (i / 6) * 270
		rects = append(rects, image.Rect(x, y, x+320, y+270))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, r := range rects {
			sub := full.SubImage(r).(*image.RGBA)
			if _, _, err := (PNG{}).Encode(nil, sub, 85); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// The same 24 regions through the builder, which now encodes them at once. This
// is the number that matters: it is what the frame loop actually spends.
func BenchmarkScrollRegionsThroughBuilder(b *testing.B) {
	full := makeDesktop(1920, 1080)
	acc := NewAccumulator(image.Rect(0, 0, 1920, 1080), DefaultMaxRects, DefaultFullFraction)
	for i := 0; i < 24; i++ {
		x := (i % 6) * 320
		y := (i / 6) * 270
		acc.Add(image.Rect(x, y, x+320, y+270))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := NewBuilder(nil).Build(full, acc, 85); err != nil {
			b.Fatal(err)
		}
	}
}

// The same workload below the point where the accumulator decides the whole
// desktop changed. Eight regions merge into one rectangle, which is worth
// knowing before reading anything into these numbers: there is often nothing to
// spread out, because the merge has already done it.
func BenchmarkScrollRegionsFewer(b *testing.B) {
	full := makeDesktop(1920, 1080)
	acc := NewAccumulator(image.Rect(0, 0, 1920, 1080), DefaultMaxRects, DefaultFullFraction)
	for i := 0; i < 8; i++ {
		x := (i % 4) * 320
		y := (i / 4) * 270
		acc.Add(image.Rect(x, y, x+320, y+270))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := NewBuilder(nil).Build(full, acc, 85); err != nil {
			b.Fatal(err)
		}
	}
}
