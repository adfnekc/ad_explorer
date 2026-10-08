package session

import (
	"image"
	"sync"

	"github.com/adfnekc/ad_explorer/internal/rdp"
)

// FrameSource is where a session gets pixels.
//
// It is deliberately narrower than grdp's framebuffer: the session needs to know
// the desktop size and to be handed batches of changes it can keep for as long
// as it needs. That keeps the encoder testable with a synthetic source and keeps
// the grdp-specific details in one adapter.
type FrameSource interface {
	// Bounds is the size of the desktop.
	Bounds() image.Rectangle
	// Updates delivers batches. The channel is closed when the source stops.
	Updates() <-chan Update
}

// Update is one batch of changes: the desktop as of that moment, and the regions
// that changed in it.
//
// The image belongs to the consumer until Release is called. That ownership is
// the whole point of the type: an earlier version handed out a buffer the source
// kept drawing into, so the encoder could read a half drawn frame, which -race
// reports as a data race and a person sees as tearing.
//
// A source keeps a small pool and cannot reuse a buffer until it comes back, so
// a slow encoder leaves the source with nothing to hand over. That is where the
// backpressure comes from, and it is why a source carries the regions forward to
// its next update rather than dropping them.
type Update struct {
	Image   image.Image
	Regions []image.Rectangle

	release func()
}

// Release returns the image to its source. It is safe to call more than once.
func (u *Update) Release() {
	if u != nil && u.release != nil {
		u.release()
		u.release = nil
	}
}

// InputObserver is implemented by a frame source that wants to draw what the
// session sent to the remote.
//
// It exists so the demo source can show keystrokes and clicks on the synthetic
// desktop, which is the only way to see that the input mapping is right without
// a picture from a real host. A real source simply does not implement it.
type InputObserver interface {
	ObserveKey(code string, down bool)
	ObserveMouse(action string, button, x, y int)
	ObserveWheel(delta, x, y int)
	ObserveText(text string)
}

// frameSourceProvider is implemented by a remote that can supply pixels
// directly. The demo remote does; the grdp adapter below wraps the grdp
// framebuffer instead.
type frameSourceProvider interface {
	FrameSource() (FrameSource, error)
}

// defaultUpdateBuffer is how many batches a source may have in flight. Two is
// enough for the encoder to work on one while the next is prepared, and small
// enough that a stalled encoder makes the source carry regions forward instead
// of building a backlog of stale frames.
const defaultUpdateBuffer = 2

// poolBuffers is how many desktop buffers exist. It is one more than the update
// depth because the session keeps the last frame it encoded, so that a client
// attaching to a desktop nothing is drawing on can still be repainted. Sizing
// the pool to the update depth would leave the source with nothing free and it
// would stop publishing.
const poolBuffers = defaultUpdateBuffer + 1

// framePool hands out desktop-sized buffers and takes them back.
//
// Without it every update would allocate the whole desktop: a 1280x720 frame is
// 3.7 MiB, which at 30 frames a second is more allocation than the garbage
// collector should be asked to keep up with.
type framePool struct {
	mu   sync.Mutex
	free []*image.RGBA
	max  int
}

func newFramePool(max int) *framePool {
	if max < 1 {
		max = 1
	}
	return &framePool{max: max}
}

// take returns a buffer of the given size, or nil when every buffer is still
// with a consumer and the pool is at its cap.
func (p *framePool) take(bounds image.Rectangle) *image.RGBA {
	if bounds.Empty() {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// Buffers of the wrong size are dropped rather than resized: that only
	// happens after a display control resize, which is rare.
	kept := p.free[:0]
	var picked *image.RGBA
	for _, b := range p.free {
		if b.Bounds() != bounds {
			continue
		}
		if picked == nil {
			picked = b
			continue
		}
		kept = append(kept, b)
	}
	p.free = kept

	if picked != nil {
		return picked
	}
	if len(p.free) >= p.max {
		return nil
	}
	return image.NewRGBA(bounds)
}

// give returns a buffer for reuse.
func (p *framePool) give(b *image.RGBA) {
	if b == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) < p.max {
		p.free = append(p.free, b)
	}
}

// reset drops every buffer, which is what a change of desktop size needs.
func (p *framePool) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = nil
}

// grdpSource reads the desktop out of grdp's framebuffer.
//
// It keeps its own copy because grdp's buffer has a much narrower life than the
// encoder needs: grdp's pixels are only valid until its callback returns, while
// the encoder runs later, possibly across a WebSocket write. The copy is applied
// per changed region, so only the dirty pixels move.
type grdpSource struct {
	remote remote

	mu     sync.Mutex
	cur    *image.RGBA
	bounds image.Rectangle
	pool   *framePool

	updates chan Update
}

// grdpFrames builds a frame source over the grdp framebuffer.
func grdpFrames(r remote) (FrameSource, error) {
	fb, err := r.Framebuffer()
	if err != nil {
		return nil, err
	}
	bounds := fb.Bounds()
	if bounds.Empty() {
		return nil, rdp.ErrNotReady
	}

	s := &grdpSource{
		remote:  r,
		cur:     image.NewRGBA(bounds),
		bounds:  bounds,
		pool:    newFramePool(poolBuffers),
		updates: make(chan Update, defaultUpdateBuffer),
	}
	if err := r.OnFrame(s.onFrame); err != nil {
		return nil, err
	}
	return s, nil
}

// Bounds implements FrameSource.
func (s *grdpSource) Bounds() image.Rectangle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bounds
}

// Updates implements FrameSource.
func (s *grdpSource) Updates() <-chan Update { return s.updates }

// onFrame runs on grdp's decode goroutine, and grdp's pixels are only valid
// until it returns. Everything that reads them happens here; everything slow
// happens after the regions have been copied.
func (s *grdpSource) onFrame(dirty []image.Rectangle) {
	fb, err := s.remote.Framebuffer()
	if err != nil {
		return
	}
	bounds := fb.Bounds()
	if bounds.Empty() {
		return
	}

	s.mu.Lock()
	if bounds != s.bounds {
		// Display Control changed the desktop. The old copy is the wrong shape
		// and every coordinate in flight refers to it, so it is replaced and
		// the whole of the new one is what changed.
		s.cur = image.NewRGBA(bounds)
		s.bounds = bounds
		s.pool.reset()
		dirty = []image.Rectangle{bounds}
	}

	pix, stride := fb.Pix(), fb.Stride()
	for _, r := range dirty {
		copyBGRA(s.cur, pix, stride, r.Intersect(s.bounds))
	}

	buf := s.pool.take(s.bounds)
	if buf == nil {
		// Every buffer is with the encoder. The pixels are already in our own
		// copy, so nothing is lost by waiting: the next callback republishes
		// this region along with its own, because the desktop copy still has
		// both.
		s.mu.Unlock()
		return
	}
	copy(buf.Pix, s.cur.Pix)
	regions := clipAll(dirty, s.bounds)
	s.mu.Unlock()

	update := Update{
		Image:   buf,
		Regions: regions,
		release: func() { s.pool.give(buf) },
	}
	select {
	case s.updates <- update:
	default:
		// Nothing is draining the channel; give the buffer straight back rather
		// than holding it until the next update.
		update.Release()
	}
}

// copyBGRA copies one region out of grdp's buffer into an RGBA image.
//
// The conversion walks the bytes rather than going through At(), which is the
// difference between a memcpy-shaped loop and roughly a million interface calls
// for a full 1280x720 frame.
func copyBGRA(dst *image.RGBA, src []byte, srcStride int, r image.Rectangle) {
	if r.Empty() {
		return
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		rowStart := y*srcStride + r.Min.X*4
		if rowStart < 0 || rowStart+r.Dx()*4 > len(src) {
			return
		}
		di := dst.PixOffset(r.Min.X, y)
		for x := 0; x < r.Dx(); x++ {
			so := rowStart + x*4
			dst.Pix[di] = src[so+2] // R, from the third byte of BGRA
			dst.Pix[di+1] = src[so+1]
			dst.Pix[di+2] = src[so] // B
			dst.Pix[di+3] = 0xff
			di += 4
		}
	}
}

// clipAll clips a batch of regions to the desktop and drops the empty ones.
func clipAll(regions []image.Rectangle, bounds image.Rectangle) []image.Rectangle {
	out := make([]image.Rectangle, 0, len(regions))
	for _, r := range regions {
		if r = r.Intersect(bounds); !r.Empty() {
			out = append(out, r)
		}
	}
	return out
}
