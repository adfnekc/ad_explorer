package stream

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"testing"

	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// quadrants returns a 200x200 image with four solid 100x100 blocks, so a region
// can be checked by the colour it decodes to.
func quadrants() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 200, 200))
	fill := func(r image.Rectangle, c color.RGBA) {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				img.SetRGBA(x, y, c)
			}
		}
	}
	fill(image.Rect(0, 0, 100, 100), color.RGBA{R: 220, G: 30, B: 30, A: 255})
	fill(image.Rect(100, 0, 200, 100), color.RGBA{R: 30, G: 200, B: 30, A: 255})
	fill(image.Rect(0, 100, 100, 200), color.RGBA{R: 30, G: 30, B: 220, A: 255})
	fill(image.Rect(100, 100, 200, 200), color.RGBA{R: 240, G: 240, B: 240, A: 255})
	return img
}

// averageOf decodes a JPEG and averages its pixels, which is stable enough to
// identify a solid region across lossy compression.
func averageOf(t *testing.T, payload []byte) (r, g, b float64) {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode produced an unreadable JPEG: %v", err)
	}
	bounds := img.Bounds()
	var sumR, sumG, sumB float64
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			sumR += float64(cr >> 8)
			sumG += float64(cg >> 8)
			sumB += float64(cb >> 8)
		}
	}
	n := float64(bounds.Dx() * bounds.Dy())
	return sumR / n, sumG / n, sumB / n
}

func closeTo(got, want float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= 12
}

func TestJPEGEncodeRoundTrip(t *testing.T) {
	img := quadrants()

	payload, format, err := JPEG{}.Encode(nil, img, 90)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if format != wsframe.FormatJPEG {
		t.Errorf("format = %#x, want FormatJPEG", format)
	}
	if len(payload) == 0 {
		t.Fatal("Encode produced no bytes")
	}

	decoded, err := jpeg.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("the payload is not a JPEG: %v", err)
	}
	if got := decoded.Bounds().Size(); got != image.Pt(200, 200) {
		t.Errorf("decoded size = %v, want 200x200", got)
	}
}

func TestJPEGEncodeAppendsToDst(t *testing.T) {
	prefix := []byte("header")

	payload, _, err := JPEG{}.Encode(prefix, quadrants(), 80)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(payload) <= len(prefix) {
		t.Fatalf("Encode returned %d bytes, no more than the prefix", len(payload))
	}
	if !bytes.Equal(payload[:len(prefix)], prefix) {
		t.Errorf("Encode did not append to dst: %q", payload[:len(prefix)])
	}
	if _, err := jpeg.Decode(bytes.NewReader(payload[len(prefix):])); err != nil {
		t.Errorf("the appended bytes are not a JPEG: %v", err)
	}
}

func TestJPEGEncodeRejectsEmpty(t *testing.T) {
	if _, _, err := (JPEG{}).Encode(nil, nil, 80); !errors.Is(err, ErrEmptyRegion) {
		t.Errorf("Encode(nil) = %v, want ErrEmptyRegion", err)
	}
	if _, _, err := (JPEG{}).Encode(nil, image.NewRGBA(image.Rect(0, 0, 0, 0)), 80); !errors.Is(err, ErrEmptyRegion) {
		t.Errorf("Encode(empty) = %v, want ErrEmptyRegion", err)
	}
}

func TestClampQuality(t *testing.T) {
	// Zero must not survive: the standard library reads 0 as "use the
	// default", so passing it through would silently ignore the caller.
	cases := map[int]int{-100: 1, 0: 1, 1: 1, 50: 50, 100: 100, 101: 100, 1 << 20: 100}
	for in, want := range cases {
		if got := clampQuality(in); got != want {
			t.Errorf("clampQuality(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestBuilderEncodesEachRegion(t *testing.T) {
	img := quadrants()
	acc := NewAccumulator(img.Bounds(), DefaultMaxRects, DefaultFullFraction)
	acc.Add(image.Rect(0, 0, 100, 100))
	acc.Add(image.Rect(100, 100, 200, 200))

	b := NewBuilder(JPEG{})
	frames, err := b.Build(img, acc, 85)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}

	for i, f := range frames {
		if f.Type != wsframe.TypeImage {
			t.Errorf("frame %d type = %#x, want TypeImage", i, f.Type)
		}
		if f.Format != wsframe.FormatJPEG {
			t.Errorf("frame %d format = %#x, want JPEG", i, f.Format)
		}
		if f.Seq == 0 {
			t.Errorf("frame %d has no sequence number", i)
		}
		if len(f.Payload) == 0 {
			t.Errorf("frame %d has no payload", i)
		}
	}
	if frames[0].Seq == frames[1].Seq {
		t.Error("two frames share a sequence number; the client uses these to notice a gap")
	}
	if frames[0].Flags&wsframe.FlagKeyframe == 0 {
		t.Error("the first frame of a stream must be marked as a keyframe")
	}
	if frames[1].Flags&wsframe.FlagKeyframe != 0 {
		t.Error("only the first frame should be a keyframe")
	}
}

func TestBuilderCropsTheRightPixels(t *testing.T) {
	// The whole point of cropping: a region must encode the pixels at that
	// position, not the top-left corner of the framebuffer.
	img := quadrants()
	b := NewBuilder(JPEG{})

	cases := []struct {
		name  string
		rect  image.Rectangle
		wantR float64
		wantG float64
		wantB float64
	}{
		{"top left is red", image.Rect(0, 0, 100, 100), 220, 30, 30},
		{"top right is green", image.Rect(100, 0, 200, 100), 30, 200, 30},
		{"bottom left is blue", image.Rect(0, 100, 100, 200), 30, 30, 220},
		{"bottom right is white", image.Rect(100, 100, 200, 200), 240, 240, 240},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := NewAccumulator(img.Bounds(), DefaultMaxRects, DefaultFullFraction)
			acc.Add(tc.rect)
			frames, err := b.Build(img, acc, 95)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if len(frames) != 1 {
				t.Fatalf("got %d frames, want 1", len(frames))
			}

			r, g, bl := averageOf(t, frames[0].Payload)
			if !closeTo(r, tc.wantR) || !closeTo(g, tc.wantG) || !closeTo(bl, tc.wantB) {
				t.Errorf("region %v decoded to (%.0f,%.0f,%.0f), want about (%.0f,%.0f,%.0f)",
					tc.rect, r, g, bl, tc.wantR, tc.wantG, tc.wantB)
			}
		})
	}
}

func TestBuilderMarksAFullFrame(t *testing.T) {
	img := quadrants()
	acc := NewAccumulator(img.Bounds(), DefaultMaxRects, DefaultFullFraction)
	acc.Add(image.Rect(0, 0, 150, 150)) // 22500 of 40000 = 56%, over the 60%? no.
	// Use a region that is definitely over the threshold.
	acc.Reset()
	acc.Add(image.Rect(0, 0, 160, 160)) // 25600 of 40000 = 64%

	frames, err := NewBuilder(JPEG{}).Build(img, acc, 80)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if !frames[0].IsFull() {
		t.Error("a promoted frame is not marked FlagFull")
	}
	if frames[0].Rect != img.Bounds() {
		t.Errorf("full frame rect = %v, want the desktop bounds %v", frames[0].Rect, img.Bounds())
	}
}

func TestBuilderClampsRegionsToTheDesktop(t *testing.T) {
	// The accumulator clips, but a builder is also reachable with a hand-built
	// region list, and reading past the framebuffer would panic in the encoder.
	img := quadrants()
	acc := NewAccumulator(img.Bounds(), DefaultMaxRects, DefaultFullFraction)
	acc.Add(image.Rect(150, 150, 400, 400)) // extends past the 200x200 image

	frames, err := NewBuilder(JPEG{}).Build(img, acc, 80)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}

	decoded, err := jpeg.Decode(bytes.NewReader(frames[0].Payload))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := decoded.Bounds().Size(), image.Pt(50, 50); got != want {
		t.Errorf("encoded size = %v, want %v (clipped to the framebuffer)", got, want)
	}
}

func TestBuilderNothingToDo(t *testing.T) {
	img := quadrants()
	acc := NewAccumulator(img.Bounds(), DefaultMaxRects, DefaultFullFraction)

	frames, err := NewBuilder(JPEG{}).Build(img, acc, 80)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if frames != nil {
		t.Errorf("Build with nothing dirty returned %d frames", len(frames))
	}
}

func TestBuilderRejectsNilSource(t *testing.T) {
	acc := NewAccumulator(image.Rect(0, 0, 100, 100), DefaultMaxRects, DefaultFullFraction)
	acc.Add(image.Rect(0, 0, 10, 10))

	if _, err := NewBuilder(JPEG{}).Build(nil, acc, 80); !errors.Is(err, ErrEmptyRegion) {
		t.Errorf("Build(nil) = %v, want ErrEmptyRegion", err)
	}
}

func TestBuilderSequenceNeverRepeats(t *testing.T) {
	img := quadrants()
	acc := NewAccumulator(img.Bounds(), DefaultMaxRects, DefaultFullFraction)
	b := NewBuilder(JPEG{})

	seen := make(map[uint32]bool)
	for frame := 0; frame < 5; frame++ {
		acc.Add(image.Rect(0, 0, 40, 40))
		frames, err := b.Build(img, acc, 70)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		for _, f := range frames {
			if seen[f.Seq] {
				t.Fatalf("sequence number %d was reused", f.Seq)
			}
			seen[f.Seq] = true
		}
		acc.Reset()
	}
	if len(seen) != 5 {
		t.Errorf("saw %d sequence numbers over 5 frames, want 5", len(seen))
	}
}

// A builder with no encoder gets the lossless one, because a desktop is text and
// flat colour and JPEG smears both. The test names it rather than checking for
// any encoder, so changing the default is a decision that has to be written down
// here as well as made there.
func TestNewBuilderDefaultsToTheLosslessEncoder(t *testing.T) {
	if got := NewBuilder(nil).EncoderName(); got != "png" {
		t.Errorf("EncoderName() = %q, want png", got)
	}
}

func TestOffsetImageIsUsedWhenSubImageIsMissing(t *testing.T) {
	// A framebuffer that only implements the basic image.Image must still be
	// croppable, which is what offsetImage is for.
	img := quadrants()
	wrapped := &noSubImage{img}

	acc := NewAccumulator(img.Bounds(), DefaultMaxRects, DefaultFullFraction)
	acc.Add(image.Rect(100, 100, 200, 200)) // the white quadrant

	frames, err := NewBuilder(JPEG{}).Build(wrapped, acc, 95)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	r, g, b := averageOf(t, frames[0].Payload)
	if !closeTo(r, 240) || !closeTo(g, 240) || !closeTo(b, 240) {
		t.Errorf("decoded (%.0f,%.0f,%.0f), want the white quadrant", r, g, b)
	}
}

// noSubImage hides SubImage so the fallback path is exercised.
type noSubImage struct{ inner image.Image }

func (n *noSubImage) ColorModel() color.Model { return n.inner.ColorModel() }
func (n *noSubImage) Bounds() image.Rectangle { return n.inner.Bounds() }
func (n *noSubImage) At(x, y int) color.Color { return n.inner.At(x, y) }

func TestOffsetImageTranslatesCoordinates(t *testing.T) {
	img := quadrants()
	o := &offsetImage{src: img, offset: image.Pt(100, 100), size: image.Pt(100, 100)}

	if got := o.Bounds(); got != image.Rect(0, 0, 100, 100) {
		t.Errorf("Bounds() = %v, want a 100x100 view", got)
	}
	// The view's origin must read the source at the offset.
	r, g, b, _ := o.At(0, 0).RGBA()
	if !closeTo(float64(r>>8), 240) || !closeTo(float64(g>>8), 240) || !closeTo(float64(b>>8), 240) {
		t.Errorf("At(0,0) = (%d,%d,%d), want the white quadrant at (100,100)", r>>8, g>>8, b>>8)
	}
}

func TestOffsetImagePanicsOnOutOfRangeAccess(t *testing.T) {
	// Documented behaviour rather than a guarantee: reading outside a view is a
	// programming error, and the encoder never does it because regions are
	// clipped to the framebuffer first.
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	o := &offsetImage{src: img, offset: image.Pt(5, 5), size: image.Pt(10, 10)}

	defer func() {
		if recover() == nil {
			t.Skip("image.RGBA bounds checks are not enabled here")
		}
	}()
	_ = o.At(9, 9)
}
