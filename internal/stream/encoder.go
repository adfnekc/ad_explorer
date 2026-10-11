package stream

import (
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"runtime"
	"sync"

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

	// Clipping is done first and on its own, because it is cheap and because it
	// decides how many regions there are to spread out. The sequence numbers are
	// handed out here too, in this order, so that encoding them in a different
	// order cannot change the numbers the client sees: a gap in the sequence is
	// how the client knows a frame went missing.
	work := make([]image.Rectangle, 0, len(rects))
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
		work = append(work, r)
	}
	if len(work) == 0 {
		return nil, nil
	}

	// A large region is one encode and cannot be spread out, so it is divided
	// before anything is encoded. The strips are regions like any other and the
	// client draws them in the order they arrive, which is what makes the split
	// invisible in the result — the same pixels, drawn by halves rather than at
	// once. This is where a 1080p frame gets its parallel work from: as one
	// rectangle it is 58 ms, which is nearly four frame intervals.
	//
	// Only when there is nothing else to encode. When a frame already has many
	// regions there is work enough to spread out, and cutting them up as well
	// would produce more, smaller messages for no more parallelism.
	if len(work) == 1 {
		work = splitForParallelism(work[0], bounds)
	}
	first := b.seq + 1
	b.seq += uint32(len(work))

	build := func(i int) (wsframe.Frame, error) {
		region, err := crop(src, work[i], bounds)
		if err != nil {
			return wsframe.Frame{}, err
		}
		payload, format, err := b.enc.Encode(nil, region, quality)
		if err != nil {
			return wsframe.Frame{}, err
		}
		var flags uint16
		// The flag is on the first frame of the batch and nowhere else. It says
		// a whole desktop is on its way, which is what the client clears its
		// queue for — and clearing is not additive, so a second frame carrying it
		// would throw away the strips of the same desktop that had not been drawn
		// yet. They are not lost from the picture: the next full frame brings
		// them back, one frame interval later, which is late enough to be seen.
		if full && i == 0 {
			flags |= wsframe.FlagFull
		}
		if first+uint32(i) == 1 {
			flags |= wsframe.FlagKeyframe
		}
		return wsframe.Frame{
			Type:    wsframe.TypeImage,
			Format:  format,
			Flags:   flags,
			Rect:    work[i],
			Seq:     first + uint32(i),
			Payload: payload,
		}, nil
	}

	// The regions do not depend on each other, and are put back in the order they
	// were clipped into because that order is what decides which of two
	// overlapping regions wins on the client. One region is one goroutine's worth
	// of work and not two, so a plain loop is kept for the common case: a caret
	// or a clock is usually all that changed, and spreading one waking per frame
	// across the machine to encode it would cost more than it saves.
	if len(work) == 1 {
		frame, err := build(0)
		if err != nil {
			return nil, err
		}
		return []wsframe.Frame{frame}, nil
	}

	frames := make([]wsframe.Frame, len(work))
	errs := make([]error, len(work))
	workers := runtime.GOMAXPROCS(0)
	if workers > len(work) {
		workers = len(work)
	}
	// No fewer than two: this path is only taken when there is more than one
	// region, and a machine reported as having one processor would otherwise do
	// the work twice as slowly through a channel for nothing.
	if workers < 2 {
		workers = 2
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range work {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			frames[i], errs[i] = build(i)
		}(i)
	}
	wg.Wait()

	// Reported in the order they were asked for, so which region is named when
	// several fail does not depend on which goroutine lost the race.
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return frames, nil
}

// stripArea is the area above which a region is worth dividing. It is a little
// under half of a 1080p desktop, so a window being dragged across the screen is
// split and a dialog is not: dividing a small region costs a goroutine and a
// message for each piece and saves an amount of time too small to measure.
const stripArea = 900 * 500

// stripRows is the shortest strip worth making. A strip is an encoded message
// with a header and a rectangle of its own, so very thin ones cost more in
// framing than they save in time, and PNG has less to work with in a short run
// of rows than in a tall one.
const stripRows = 32

// splitForParallelism divides a large region into horizontal strips so that the
// encoder has more than one piece of work to run at once. It returns the region
// untouched when it is small enough that dividing it would not pay.
func splitForParallelism(r image.Rectangle, desktop image.Rectangle) []image.Rectangle {
	height := r.Dy()
	if height <= stripRows*2 || r.Dx()*height < stripArea {
		return []image.Rectangle{r}
	}
	// No more strips than the machine can run at once, and no fewer than two,
	// and never thin enough to fall under stripRows. Dividing into more pieces
	// than there are processors does not finish sooner: what is left over waits
	// for a processor just as it would have waited for the one goroutine.
	pieces := runtime.GOMAXPROCS(0)
	if pieces > height/stripRows {
		pieces = height / stripRows
	}
	if pieces < 2 {
		return []image.Rectangle{r}
	}
	// Rounded up, so every strip is at least stripRows tall and the last one
	// takes the remainder rather than being a sliver.
	rows := (height + pieces - 1) / pieces
	if rows < stripRows {
		rows = stripRows
	}

	strips := make([]image.Rectangle, 0, pieces+1)
	for y := r.Min.Y; y < r.Max.Y; y += rows {
		end := y + rows
		if end > r.Max.Y {
			end = r.Max.Y
		}
		strips = append(strips, image.Rect(r.Min.X, y, r.Max.X, end).Intersect(desktop))
	}
	return strips
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
