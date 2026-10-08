package stream

import (
	"image"
	"testing"
)

// desktop is a 1000x1000 screen, so 60% of it is 600000 pixels and the
// arithmetic in these tests is easy to check by eye.
var desktop = image.Rect(0, 0, 1000, 1000)

func newAcc(maxRects int) *Accumulator {
	return NewAccumulator(desktop, maxRects, DefaultFullFraction)
}

func TestAccumulatorEmpty(t *testing.T) {
	a := newAcc(0)
	if !a.Empty() {
		t.Error("a fresh accumulator is not empty")
	}
	rects, full := a.Result()
	if len(rects) != 0 || full {
		t.Errorf("Result() = %v, %v; want no regions and not full", rects, full)
	}
}

func TestAccumulatorSingleRegion(t *testing.T) {
	a := newAcc(0)
	a.Add(image.Rect(10, 20, 110, 120))

	if a.Empty() {
		t.Fatal("Empty() = true after Add")
	}
	rects, full := a.Result()
	if full {
		t.Fatal("a 100x100 region promoted to a full frame")
	}
	if len(rects) != 1 || rects[0] != image.Rect(10, 20, 110, 120) {
		t.Errorf("Result() = %v, want [%v]", rects, image.Rect(10, 20, 110, 120))
	}
}

func TestAccumulatorMergesOverlapping(t *testing.T) {
	a := newAcc(0)
	a.Add(image.Rect(0, 0, 100, 100))
	a.Add(image.Rect(50, 50, 150, 150))

	rects, full := a.Result()
	if full {
		t.Fatal("promoted to a full frame")
	}
	if len(rects) != 1 || rects[0] != image.Rect(0, 0, 150, 150) {
		t.Errorf("Result() = %v, want one union region", rects)
	}
}

func TestAccumulatorAbsorbsContainedRegion(t *testing.T) {
	a := newAcc(0)
	a.Add(image.Rect(0, 0, 200, 200))
	a.Add(image.Rect(50, 50, 100, 100))

	rects, _ := a.Result()
	if len(rects) != 1 || rects[0] != image.Rect(0, 0, 200, 200) {
		t.Errorf("Result() = %v, want the outer region alone: the inner one costs nothing to include", rects)
	}
}

func TestAccumulatorMergesEdgeAdjacentRegions(t *testing.T) {
	// Two regions that share an edge cost one encode instead of two for
	// exactly the same pixels, so they must be merged even though they do not
	// overlap.
	a := newAcc(0)
	a.Add(image.Rect(0, 0, 100, 100))
	a.Add(image.Rect(100, 0, 200, 100))

	rects, _ := a.Result()
	if len(rects) != 1 || rects[0] != image.Rect(0, 0, 200, 100) {
		t.Errorf("Result() = %v, want one merged region", rects)
	}
}

func TestAccumulatorKeepsDistantRegionsApart(t *testing.T) {
	// The whole point of tracking regions instead of one bounding box: a caret
	// in the top left and a clock in the bottom right must not turn every frame
	// into a full-screen encode.
	a := newAcc(0)
	a.Add(image.Rect(0, 0, 100, 100))
	a.Add(image.Rect(900, 900, 1000, 1000))

	rects, full := a.Result()
	if full {
		t.Fatal("two small distant regions promoted to a full frame")
	}
	if len(rects) != 2 {
		t.Errorf("Result() = %v, want two separate regions", rects)
	}
}

func TestAccumulatorRespectsMaxRects(t *testing.T) {
	a := newAcc(3)

	// Five well-separated regions, each too small to trip the full threshold.
	// Merging any two of them wastes area, so this exercises the cap rather
	// than the free merges.
	origins := []image.Point{{0, 0}, {200, 0}, {400, 0}, {600, 0}, {800, 0}}
	for _, o := range origins {
		a.Add(image.Rect(o.X, o.Y, o.X+40, o.Y+40))
	}

	rects, full := a.Result()
	if full {
		t.Fatal("promoted to a full frame")
	}
	if len(rects) > 3 {
		t.Errorf("Result() returned %d regions, want at most 3", len(rects))
	}
	if len(rects) == 0 {
		t.Fatal("Result() returned nothing")
	}
}

func TestAccumulatorCheapestPairIsMergedFirst(t *testing.T) {
	// With room for only two regions, the pair that wastes the least area must
	// be merged and the far one left alone. Picking the wrong pair here is what
	// makes an encoder send a full screen for a blinking caret.
	a := newAcc(2)

	near1 := image.Rect(0, 0, 100, 100)
	near2 := image.Rect(150, 0, 250, 100) // a 50px gap: 5000 wasted pixels
	far := image.Rect(900, 900, 1000, 1000)

	a.Add(near1)
	a.Add(near2)
	a.Add(far)

	rects, _ := a.Result()
	if len(rects) != 2 {
		t.Fatalf("Result() = %v, want two regions", rects)
	}

	// One region must be the merge of the two near ones, and the other the far
	// one, untouched.
	merged := image.Rect(0, 0, 250, 100)
	var sawMerged, sawFar bool
	for _, r := range rects {
		switch r {
		case merged:
			sawMerged = true
		case far:
			sawFar = true
		}
	}
	if !sawMerged || !sawFar {
		t.Errorf("Result() = %v, want %v and %v", rects, merged, far)
	}
}

func TestAccumulatorPromotesLargeSingleRegion(t *testing.T) {
	a := newAcc(0)
	a.Add(image.Rect(0, 0, 800, 800)) // 640000 >= 60% of 1000000

	if a.Empty() {
		t.Error("Empty() = true after a region that should promote to full")
	}
	rects, full := a.Result()
	if !full {
		t.Fatal("a region covering 64% of the desktop did not promote to a full frame")
	}
	if rects != nil {
		t.Errorf("rects = %v, want nil when full: the caller sends the desktop bounds", rects)
	}
}

func TestAccumulatorPromotesWhenTheTotalCrossesTheThreshold(t *testing.T) {
	// Four disjoint 400x400 regions, 160000 pixels each, none close enough to
	// another to merge for free. The running total is what prompts the
	// promotion, not any single region.
	a := newAcc(0)
	regions := []image.Rectangle{
		image.Rect(0, 0, 400, 400),
		image.Rect(500, 0, 900, 400),
		image.Rect(0, 500, 400, 900),
		image.Rect(500, 500, 900, 900),
	}

	for i, r := range regions[:3] {
		a.Add(r)
		if _, full := a.Result(); full {
			t.Fatalf("promoted after %d regions (%d of 600000 pixels)", i+1, (i+1)*160000)
		}
	}
	if got := len(a.rects); got != 3 {
		t.Fatalf("regions merged when they should not have: %d left", got)
	}

	a.Add(regions[3])
	if _, full := a.Result(); !full {
		t.Error("640000 pixels is above the 600000 threshold and must promote to a full frame")
	}
}

func TestAccumulatorThresholdIsInclusive(t *testing.T) {
	// Exactly 60% of the desktop: a 1000x600 region is 600000 pixels.
	a := newAcc(0)
	a.Add(image.Rect(0, 0, 1000, 600))
	if _, full := a.Result(); !full {
		t.Error("a region of exactly the threshold did not promote")
	}

	// One pixel short must not promote.
	b := newAcc(0)
	b.Add(image.Rect(0, 0, 1000, 599))
	if _, full := b.Result(); full {
		t.Error("a region one row short of the threshold promoted")
	}
}

func TestAccumulatorClipsToDesktop(t *testing.T) {
	t.Run("entirely outside is ignored", func(t *testing.T) {
		a := newAcc(0)
		a.Add(image.Rect(2000, 2000, 2100, 2100))
		if !a.Empty() {
			t.Errorf("Result() = %v, want nothing: the region is off-screen", a.rects)
		}
	})

	t.Run("entirely negative is ignored", func(t *testing.T) {
		a := newAcc(0)
		a.Add(image.Rect(-100, -100, -10, -10))
		if !a.Empty() {
			t.Errorf("Result() = %v, want nothing", a.rects)
		}
	})

	t.Run("partially outside is clipped", func(t *testing.T) {
		a := newAcc(0)
		a.Add(image.Rect(900, 900, 1200, 1200))
		rects, _ := a.Result()
		if len(rects) != 1 || rects[0] != image.Rect(900, 900, 1000, 1000) {
			t.Errorf("Result() = %v, want the region clipped to the desktop", rects)
		}
	})
}

func TestAccumulatorIgnoresAddAfterFull(t *testing.T) {
	a := newAcc(0)
	a.Add(image.Rect(0, 0, 800, 800))
	if _, full := a.Result(); !full {
		t.Fatal("setup failed")
	}

	// Once the whole frame is going out there is nothing left to track, so
	// later regions must not resurrect a region list.
	a.Add(image.Rect(10, 10, 20, 20))
	rects, full := a.Result()
	if !full || rects != nil {
		t.Errorf("Result() = %v, %v; want nil, true", rects, full)
	}
}

func TestAccumulatorReset(t *testing.T) {
	a := newAcc(0)
	a.Add(image.Rect(0, 0, 800, 800)) // full
	a.Reset()

	if !a.Empty() {
		t.Error("Empty() = false after Reset")
	}
	if _, full := a.Result(); full {
		t.Error("still full after Reset")
	}

	a.Add(image.Rect(10, 10, 20, 20))
	rects, full := a.Result()
	if full || len(rects) != 1 {
		t.Errorf("Result() = %v, %v after Reset and one Add; want one region, not full", rects, full)
	}
}

func TestAccumulatorResultIsACopy(t *testing.T) {
	// The session layer keeps the frame it just sent while the next one is
	// being built, so a Reset must not overwrite the rectangles it handed out.
	a := newAcc(0)
	a.Add(image.Rect(0, 0, 100, 100))
	first, _ := a.Result()

	a.Reset()
	a.Add(image.Rect(500, 500, 600, 600))
	a.Add(image.Rect(600, 500, 700, 600))

	if len(first) != 1 || first[0] != image.Rect(0, 0, 100, 100) {
		t.Errorf("the earlier result changed underneath the caller: %v", first)
	}
}

func TestNewAccumulatorDefaults(t *testing.T) {
	a := NewAccumulator(desktop, 0, 0)
	if a.maxRects != DefaultMaxRects {
		t.Errorf("maxRects = %d, want the default %d", a.maxRects, DefaultMaxRects)
	}
	// DefaultFullFraction of a 1000x1000 desktop.
	if a.threshold != int64(DefaultFullFraction*1000000) {
		t.Errorf("threshold = %d, want %d", a.threshold, int64(DefaultFullFraction*1000000))
	}

	// Out-of-range fractions fall back rather than producing a threshold that
	// promotes everything or nothing.
	for _, f := range []float64{-1, 0, 1.5} {
		if got := NewAccumulator(desktop, 0, f).threshold; got != int64(DefaultFullFraction*1000000) {
			t.Errorf("fullFraction %v gave threshold %d, want the default", f, got)
		}
	}
}
