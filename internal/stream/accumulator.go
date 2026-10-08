// Package stream turns dirty rectangles into frames worth putting on a wire.
//
// Everything here is pure: given rectangles and pixels it produces bytes, with
// no goroutines, no sockets and no clock. The session layer owns the pacing,
// the buffers and the backpressure policy, so the decisions that are easy to
// get wrong are the ones that are easy to test.
package stream

import "image"

// Defaults for an Accumulator.
const (
	// DefaultMaxRects is how many separate regions one frame may carry. More
	// regions means more encodes per tick; fewer means larger ones and more
	// bytes. Eight sits well for a desktop, where the dirty areas are usually
	// a window, a caret and a clock.
	DefaultMaxRects = 8

	// DefaultFullFraction is the share of the desktop above which sending the
	// whole frame is cheaper than describing the parts. Overlapping regions
	// make the union grow faster than the sum, so past this point the merge
	// stops paying for itself.
	DefaultFullFraction = 0.6
)

// Accumulator collects the regions that changed since the last frame and
// reduces them to a small set worth encoding.
//
// It is not safe for concurrent use; the caller owns it.
type Accumulator struct {
	bounds   image.Rectangle
	maxRects int

	// threshold is the dirty area, in pixels, at or above which a full frame is
	// cheaper than describing the parts. It is precomputed because it does not
	// depend on what has been added.
	threshold int64

	rects []image.Rectangle
	full  bool
}

// NewAccumulator returns an accumulator for a desktop of the given size. A
// non-positive maxRects or fullFraction takes the default.
func NewAccumulator(bounds image.Rectangle, maxRects int, fullFraction float64) *Accumulator {
	if maxRects <= 0 {
		maxRects = DefaultMaxRects
	}
	if fullFraction <= 0 || fullFraction > 1 {
		fullFraction = DefaultFullFraction
	}
	return &Accumulator{
		bounds:    bounds,
		maxRects:  maxRects,
		threshold: int64(fullFraction * float64(int64(bounds.Dx())*int64(bounds.Dy()))),
	}
}

// Bounds is the desktop this accumulator covers.
func (a *Accumulator) Bounds() image.Rectangle { return a.bounds }

// Add records a changed region.
//
// The region is clipped to the desktop; a region entirely outside it is
// ignored, and a region covering enough of the desktop promotes the whole
// accumulator to a full frame and discards the parts, because there is no
// longer any point tracking them.
func (a *Accumulator) Add(r image.Rectangle) {
	if a.full || r.Empty() {
		return
	}
	r = r.Intersect(a.bounds)
	if r.Empty() {
		return
	}
	if a.areaOf(r) >= a.threshold {
		a.full = true
		a.rects = a.rects[:0]
		return
	}

	a.rects = append(a.rects, r)
	a.reduce()
}

// Empty reports whether nothing has changed.
func (a *Accumulator) Empty() bool { return !a.full && len(a.rects) == 0 }

// Result reports the regions to send. When full is true, rects is nil and the
// caller sends the whole desktop: the origin and size are the desktop's, not
// whatever was added.
//
// The slice is a copy. The session layer holds one frame while building the
// next, and handing out the accumulator's own backing array would let the next
// Reset overwrite a frame that has not been written yet. A handful of
// rectangles per frame is not worth that class of bug.
func (a *Accumulator) Result() (rects []image.Rectangle, full bool) {
	if a.full {
		return nil, true
	}
	if len(a.rects) == 0 {
		return nil, false
	}
	return append([]image.Rectangle(nil), a.rects...), false
}

// Reset clears the accumulator for the next frame.
func (a *Accumulator) Reset() {
	a.rects = a.rects[:0]
	a.full = false
}

func (a *Accumulator) areaOf(r image.Rectangle) int64 {
	return int64(r.Dx()) * int64(r.Dy())
}

// reduce brings the set down to something worth encoding: it merges what
// overlaps, then merges the cheapest remaining pairs until the count fits, then
// checks whether the total has grown to the point where a full frame is better.
func (a *Accumulator) reduce() {
	a.mergeCheap()

	for len(a.rects) > a.maxRects {
		i, j := a.cheapestPair()
		a.rects[i] = a.rects[i].Union(a.rects[j])
		// j is always the higher index, so deleting it cannot disturb i.
		a.rects = append(a.rects[:j], a.rects[j+1:]...)
	}

	var total int64
	for _, r := range a.rects {
		total += a.areaOf(r)
	}
	if total >= a.threshold {
		a.full = true
		a.rects = a.rects[:0]
	}
}

// mergeCheap repeatedly unions any pair that is cheaper to send as one region
// than as two, until no such pair is left.
//
// Two cases qualify. Overlapping regions must merge: they share pixels, and one
// encode of the union replaces two encodes no matter how much extra area the L
// shape adds. Disjoint regions merge only when the union wastes nothing — a
// shared edge, or one region inside another — because there the union encodes
// exactly the same pixels in one message instead of two.
//
// Two regions separated by a real gap are left alone: merging them would encode
// everything in between. That is the case this whole type exists to avoid.
func (a *Accumulator) mergeCheap() {
	for {
		merged := false
		for i := 0; i < len(a.rects) && !merged; i++ {
			for j := i + 1; j < len(a.rects); j++ {
				union := a.rects[i].Union(a.rects[j])
				if !a.rects[i].Overlaps(a.rects[j]) &&
					a.areaOf(union) > a.areaOf(a.rects[i])+a.areaOf(a.rects[j]) {
					continue
				}
				a.rects[i] = union
				a.rects = append(a.rects[:j], a.rects[j+1:]...)
				merged = true
				break
			}
		}
		if !merged {
			return
		}
	}
}

// cheapestPair returns the two indices whose union wastes the least area, so
// that merging them costs the fewest extra pixels. The first index is always
// the lower one.
func (a *Accumulator) cheapestPair() (int, int) {
	bestI, bestJ := 0, 1
	var bestWaste int64
	first := true

	for i := 0; i < len(a.rects); i++ {
		for j := i + 1; j < len(a.rects); j++ {
			u := a.rects[i].Union(a.rects[j])
			waste := a.areaOf(u) - a.areaOf(a.rects[i]) - a.areaOf(a.rects[j])
			if first || waste < bestWaste {
				first = false
				bestWaste = waste
				bestI, bestJ = i, j
			}
		}
	}
	return bestI, bestJ
}
