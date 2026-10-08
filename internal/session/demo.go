package session

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/adfnekc/ad_explorer/internal/rdp"
)

// The demo desktop.
//
// It exists so the whole pipeline — encode, transport, decode, draw, input
// mapping, clipboard — can be seen working before grdp can supply a picture.
// Nothing here is on the path a real session takes: the session's frame loop,
// encoder and transport are the same code, and only the pixels are synthetic.
//
// It is deliberately honest about what it cannot do: resizing is refused with a
// message saying the synthetic desktop has a fixed size, rather than accepting
// the request and drawing nothing.
const (
	demoWidth    = 1024
	demoHeight   = 640
	demoInterval = 100 * time.Millisecond
	demoEchoRows = 12
	// demoProtocol labels a session that is not talking to a host, so the UI can
	// say so instead of claiming a real connection.
	demoProtocol = "demo"
	// demoMouseInterval throttles pointer moves, which arrive far faster than
	// anyone can read.
	demoMouseInterval = 150 * time.Millisecond
)

// Regions of the desktop that change independently, so the encoder is exercised
// with several small dirty rectangles rather than a whole frame.
var (
	demoWindow = image.Rect(20, 20, 1004, 500)
	demoClock  = image.Rect(824, 24, 1000, 40)
	demoAnim   = image.Rect(20, 520, 1004, 560)
	demoEcho   = image.Rect(38, 196, 990, 486)
)

// demoSource renders the synthetic desktop.
//
// It keeps a canvas that only its own ticker draws into, and publishes copies of
// it. Handing the canvas out while still drawing into it is what a data race
// looks like on a screen: half of one frame and half of the next.
type demoSource struct {
	mu      sync.Mutex
	canvas  *image.RGBA
	pool    *framePool
	updates chan Update
	stop    chan struct{}

	now       func() time.Time
	tick      int
	echo      []string
	echoDirty bool
	lastMouse time.Time
	blockX    int
	dir       int
}

func newDemoSource(now func() time.Time) *demoSource {
	if now == nil {
		now = time.Now
	}
	s := &demoSource{
		canvas:  image.NewRGBA(image.Rect(0, 0, demoWidth, demoHeight)),
		pool:    newFramePool(defaultUpdateBuffer),
		updates: make(chan Update, defaultUpdateBuffer),
		stop:    make(chan struct{}),
		now:     now,
		dir:     1,
	}
	s.drawBase()
	go s.run()
	return s
}

// Bounds implements FrameSource.
func (s *demoSource) Bounds() image.Rectangle { return s.canvas.Bounds() }

// Updates implements FrameSource.
func (s *demoSource) Updates() <-chan Update { return s.updates }

// Close stops the ticker. The session calls it when the source is a Closer.
func (s *demoSource) Close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	return nil
}

// run redraws the animated parts on a fixed cadence.
func (s *demoSource) run() {
	ticker := time.NewTicker(demoInterval)
	defer ticker.Stop()
	defer close(s.updates)

	var carried []image.Rectangle
	for {
		select {
		case <-ticker.C:
			dirty := s.step()
			dirty = append(carried, dirty...)
			if len(dirty) == 0 {
				continue
			}
			if !s.publish(dirty) {
				// No buffer free: the encoder is behind. The regions are kept
				// and published with the next tick, so nothing is dropped —
				// only delayed, which is what backpressure should look like.
				carried = dirty
				continue
			}
			carried = nil
		case <-s.stop:
			return
		}
	}
}

// publish copies the changed regions of the canvas into a buffer the consumer
// can keep, and hands it over. It reports whether a buffer was available.
func (s *demoSource) publish(dirty []image.Rectangle) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	buf := s.pool.take(s.canvas.Bounds())
	if buf == nil {
		return false
	}
	copy(buf.Pix, s.canvas.Pix)

	update := Update{
		Image:   buf,
		Regions: clipAll(dirty, s.canvas.Bounds()),
		release: func() { s.pool.give(buf) },
	}
	select {
	case s.updates <- update:
		return true
	default:
		update.Release()
		return true
	}
}

// step advances the animation and returns what changed.
func (s *demoSource) step() []image.Rectangle {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.tick++
	var dirty []image.Rectangle

	s.drawAnimationLocked()
	dirty = append(dirty, demoAnim)

	// The clock changes once a second, so it is a second dirty rectangle on a
	// different cadence from the animation.
	if s.tick%10 == 0 {
		s.drawClockLocked()
		dirty = append(dirty, demoClock)
	}

	if s.echoDirty {
		s.drawEchoLocked()
		s.echoDirty = false
		dirty = append(dirty, demoEcho)
	}

	return dirty
}

// ---------------------------------------------------------------- observers

// ObserveKey implements InputObserver.
func (s *demoSource) ObserveKey(code string, down bool) {
	dir := "up  "
	if down {
		dir = "down"
	}
	s.record(fmt.Sprintf("key      %-16s %s", code, dir))
}

// ObserveMouse implements InputObserver.
func (s *demoSource) ObserveMouse(action string, button, x, y int) {
	if action == "move" {
		s.mu.Lock()
		last := s.lastMouse
		s.mu.Unlock()
		if s.now().Sub(last) < demoMouseInterval {
			return
		}
		s.mu.Lock()
		s.lastMouse = s.now()
		s.mu.Unlock()
		s.record(fmt.Sprintf("mouse    move             %4d,%4d", x, y))
		return
	}
	s.record(fmt.Sprintf("mouse    %-4s button %d    %4d,%4d", action, button, x, y))
}

// ObserveWheel implements InputObserver.
func (s *demoSource) ObserveWheel(delta, x, y int) {
	s.record(fmt.Sprintf("wheel    %+d               %4d,%4d", delta, x, y))
}

// ObserveText implements InputObserver.
func (s *demoSource) ObserveText(text string) {
	s.record(fmt.Sprintf("text     %q", text))
}

// ObserveClipboard records a paste from the local clipboard.
func (s *demoSource) ObserveClipboard(text string) {
	if len(text) > 40 {
		text = text[:40] + "…"
	}
	s.record(fmt.Sprintf("clipboard.set %q", text))
}

// echoLines returns a copy of the recorded input. It is used by the tests, and
// it is the only way to read the echo without holding the lock.
func (s *demoSource) echoLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.echo...)
}

func (s *demoSource) record(line string) {
	stamp := s.now().Format("15:04:05")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.echo = append([]string{stamp + "  " + line}, s.echo...)
	if len(s.echo) > demoEchoRows {
		s.echo = s.echo[:demoEchoRows]
	}
	s.echoDirty = true
}

// ---------------------------------------------------------------- drawing

func (s *demoSource) drawBase() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Desktop.
	draw.Draw(s.canvas, s.canvas.Bounds(), image.NewUniform(color.RGBA{R: 0, G: 128, B: 128, A: 255}),
		image.Point{}, draw.Src)

	// Window body with the classic raised edge.
	draw.Draw(s.canvas, demoWindow, image.NewUniform(color.RGBA{R: 0xD4, G: 0xD0, B: 0xC8, A: 255}),
		image.Point{}, draw.Src)
	bevel(s.canvas, demoWindow, color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}, color.RGBA{0x80, 0x80, 0x80, 0xFF})

	// Title bar.
	title := image.Rect(demoWindow.Min.X+2, demoWindow.Min.Y+2, demoWindow.Max.X-2, demoWindow.Min.Y+20)
	for x := title.Min.X; x < title.Max.X; x++ {
		t := float64(x-title.Min.X) / float64(title.Dx())
		s.canvas.SetRGBA(x, title.Min.Y, color.RGBA{R: uint8(0x0A + 0x9C*t), G: 0x24, B: uint8(0x6A + 0x86*t), A: 255})
		for y := title.Min.Y + 1; y < title.Max.Y; y++ {
			s.canvas.SetRGBA(x, y, color.RGBA{R: 0x3A, G: 0x6E, B: 0xA5, A: 255})
		}
	}
	s.textLocked(title.Min.X+4, title.Min.Y+12, color.White, "AD_EXPLORER  —  demo desktop")

	// Static explanation.
	lines := []struct {
		x, y int
		c    color.RGBA
		s    string
	}{
		{38, 60, color.RGBA{0, 0, 0x80, 255}, "This is a synthetic desktop, not a remote one."},
		{38, 78, color.RGBA{0x40, 0x40, 0x40, 255}, "It exercises the real pipeline: the same encoder, the same"},
		{38, 94, color.RGBA{0x40, 0x40, 0x40, 255}, "wire format and the same input path a remote session uses."},
		{38, 118, color.RGBA{0x40, 0x40, 0x40, 255}, "Type, click and scroll: every event is mapped to an RDP code and"},
		{38, 134, color.RGBA{0x40, 0x40, 0x40, 255}, "printed below as it arrives."},
		{38, 166, color.RGBA{0, 0, 0x80, 255}, "INPUT ECHO"},
	}
	for _, l := range lines {
		s.textLocked(l.x, l.y, l.c, l.s)
	}

	s.drawClockLocked()
	s.drawAnimationLocked()
	s.drawEchoLocked()
}

// drawClockLocked renders the title bar clock. It changes once a second, which
// gives the encoder a small dirty rectangle on a slow cadence.
func (s *demoSource) drawClockLocked() {
	draw.Draw(s.canvas, image.Rect(demoClock.Min.X, demoClock.Min.Y, demoClock.Max.X, demoClock.Max.Y),
		image.NewUniform(color.RGBA{0x3A, 0x6E, 0xB5, 255}), image.Point{}, draw.Src)
	s.textLocked(demoClock.Min.X+4, demoClock.Min.Y+12, color.White, s.now().Format("2006-01-02 15:04:05"))
}

// drawAnimationLocked moves a block along a track. It changes ten times a
// second over a region a few hundred pixels wide, which is the steady trickle
// of dirty rectangles a real desktop produces.
func (s *demoSource) drawAnimationLocked() {
	draw.Draw(s.canvas, demoAnim, image.NewUniform(color.RGBA{0xD4, 0xD0, 0xC8, 255}), image.Point{}, draw.Src)
	bevel(s.canvas, demoAnim, color.RGBA{0xFF, 0xFF, 0xFF, 255}, color.RGBA{0x80, 0x80, 0x80, 255})

	track := image.Rect(demoAnim.Min.X+8, demoAnim.Min.Y+8, demoAnim.Max.X-8, demoAnim.Max.Y-8)
	draw.Draw(s.canvas, track, image.NewUniform(color.RGBA{0xFF, 0xFF, 0xFF, 255}), image.Point{}, draw.Src)
	draw.Draw(s.canvas, image.Rect(track.Min.X, track.Min.Y, track.Max.X, track.Min.Y+1),
		image.NewUniform(color.RGBA{0x80, 0x80, 0x80, 255}), image.Point{}, draw.Src)

	const blockWidth = 40
	s.blockX += s.dir * 6
	if s.blockX <= 0 {
		s.blockX, s.dir = 0, 1
	}
	if s.blockX >= track.Dx()-blockWidth {
		s.blockX, s.dir = track.Dx()-blockWidth, -1
	}
	block := image.Rect(track.Min.X+s.blockX, track.Min.Y+2, track.Min.X+s.blockX+blockWidth, track.Max.Y-2)
	draw.Draw(s.canvas, block, image.NewUniform(color.RGBA{0, 0, 0x80, 255}), image.Point{}, draw.Src)
}

// drawEchoLocked renders the most recent input events.
func (s *demoSource) drawEchoLocked() {
	draw.Draw(s.canvas, demoEcho, image.NewUniform(color.White), image.Point{}, draw.Src)
	bevel(s.canvas, demoEcho, color.RGBA{0x80, 0x80, 0x80, 255}, color.RGBA{0xFF, 0xFF, 0xFF, 255})

	y := demoEcho.Min.Y + 16
	if len(s.echo) == 0 {
		s.textLocked(demoEcho.Min.X+8, y, color.RGBA{0x80, 0x80, 0x80, 255}, "waiting for input…")
		return
	}
	for i, line := range s.echo {
		c := color.RGBA{0, 0, 0, 255}
		if i == 0 {
			c = color.RGBA{0, 0, 0x80, 255}
		}
		s.textLocked(demoEcho.Min.X+8, y, c, line)
		y += 15
	}
}

func (s *demoSource) textLocked(x, y int, c color.Color, text string) {
	drawer := &font.Drawer{
		Dst:  s.canvas,
		Src:  image.NewUniform(c),
		Face: basicfont.Face7x13,
		Dot:  fixed.P(x, y),
	}
	drawer.DrawString(text)
}

// bevel draws the two-tone edge the classic theme uses, one pixel thick.
func bevel(img *image.RGBA, r image.Rectangle, light, dark color.RGBA) {
	for x := r.Min.X; x < r.Max.X; x++ {
		img.SetRGBA(x, r.Min.Y, light)
		img.SetRGBA(x, r.Max.Y-1, dark)
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		img.SetRGBA(r.Min.X, y, light)
		img.SetRGBA(r.Max.X-1, y, dark)
	}
}

// ---------------------------------------------------------------- demo remote

// demoRemote stands in for the grdp client in demo mode. It answers input by
// drawing it, and refuses the two things a real session refuses, so the demo
// cannot be mistaken for a finished feature.
type demoRemote struct {
	cfg    rdp.Config
	source *demoSource
	events chan rdp.Event

	// mu guards closed and the send into events, so a clipboard reply cannot
	// race the close and panic on a closed channel.
	mu     sync.Mutex
	closed bool

	filesOnce sync.Once
	files     *demoFiles
}

func newDemoRemote(cfg rdp.Config, now func() time.Time) *demoRemote {
	if now == nil {
		now = time.Now
	}
	r := &demoRemote{
		cfg:    cfg,
		source: newDemoSource(now),
		events: make(chan rdp.Event, 8),
	}
	r.push(rdp.Ready{
		Width: demoWidth, Height: demoHeight,
		Name: cfg.Name, Protocol: demoProtocol,
	})
	return r
}

// FrameSource implements frameSourceProvider.
func (r *demoRemote) FrameSource() (FrameSource, error) { return r.source, nil }

// Files implements remote. The demo has a working file transfer so the whole
// path can be seen without a remote host.
func (r *demoRemote) Files() rdp.Files {
	r.filesOnce.Do(func() { r.files = &demoFiles{} })
	return r.files
}

func (r *demoRemote) Key(code string, down bool) error {
	r.source.ObserveKey(code, down)
	return nil
}

func (r *demoRemote) KeyScancode(sc uint16, down bool) {
	r.source.ObserveKey(fmt.Sprintf("scancode %#04x", sc), down)
}

func (r *demoRemote) MouseMove(x, y int) { r.source.ObserveMouse("move", -1, x, y) }

func (r *demoRemote) MouseButton(button int, down bool, x, y int) {
	action := "up"
	if down {
		action = "down"
	}
	r.source.ObserveMouse(action, button, x, y)
}

func (r *demoRemote) MouseWheel(delta, x, y int) { r.source.ObserveWheel(delta, x, y) }

// UnicodeKey succeeds. The echo already recorded the text through the input
// observer, so there is nothing to add here: the demo shows the characters
// arriving, which is what a real session now does too.
func (r *demoRemote) UnicodeKey(ch rune) error { return nil }

func (r *demoRemote) SendCtrlAltEnd() { r.source.ObserveKey("Ctrl+Alt+End (synthesised)", true) }

func (r *demoRemote) SetClipboardText(text string) error {
	r.source.ObserveClipboard(text)
	return nil
}

// RequestClipboardText answers with a sample, so the clipboard panel and its
// copy button can be seen working. A real remote supplies its own text.
func (r *demoRemote) RequestClipboardText() error {
	r.push(rdp.ClipboardText{
		Text: "demo clipboard text — copied from the synthetic desktop at " +
			r.source.now().Format("15:04:05"),
		At: r.source.now(),
	})
	return nil
}

// push queues an event, or drops it if the client is not keeping up. The send
// and the close share the lock, which is what keeps a reply from landing on a
// closed channel.
func (r *demoRemote) push(ev rdp.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.events <- ev:
	default:
	}
}

// Framebuffer, OnFrame and OnCursor are not how the demo supplies pixels: it
// hands over a FrameSource instead. They say so rather than returning an empty
// buffer, which would look like a picture that never arrives.
func (r *demoRemote) Framebuffer() (rdp.Framebuffer, error) {
	return nil, fmt.Errorf("%w: the demo supplies pixels through a frame source", rdp.ErrUnsupported)
}

func (r *demoRemote) OnFrame(func([]image.Rectangle)) error {
	return fmt.Errorf("%w: the demo supplies pixels through a frame source", rdp.ErrUnsupported)
}

// OnCursor succeeds and never fires, so the client keeps its own pointer. A
// synthetic desktop has no pointer of its own to report.
func (r *demoRemote) OnCursor(func(rdp.Cursor)) error { return nil }

// RequestResize refuses: the synthetic desktop is a fixed size, and pretending
// to resize it would make the resize control look finished.
func (r *demoRemote) RequestResize(w, h int) error {
	return fmt.Errorf("%w: the demo desktop is a fixed %dx%d", rdp.ErrUnsupported, demoWidth, demoHeight)
}

// OnResize succeeds and never fires, for the same reason.
func (r *demoRemote) OnResize(func(w, h int)) error { return nil }

func (r *demoRemote) Events() <-chan rdp.Event { return r.events }

func (r *demoRemote) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.events)
	r.mu.Unlock()

	_ = r.source.Close()
}

// ---------------------------------------------------------------- demo files

// demoFiles is an in-memory file transfer surface.
//
// It exists so the whole of the file transfer path — offer, manifest, range
// request, chunked download — can be exercised without a remote. Offering
// files makes them appear in the remote list, and downloading one reads it
// back through the same provider the remote's paste would use, so one round
// trip covers both directions.
type demoFiles struct {
	mu       sync.Mutex
	provider rdp.FileProvider
	manifest []rdp.RemoteFile
	onRemote func([]rdp.RemoteFile)
}

// Offer implements rdp.Files.
func (f *demoFiles) Offer(p rdp.FileProvider) {
	f.mu.Lock()
	f.provider = p
	var manifest []rdp.RemoteFile
	if p != nil {
		for i, name := range p.Names() {
			size, _, isDir := p.Info(name)
			manifest = append(manifest, rdp.RemoteFile{
				Index: uint32(i), Name: name, Size: size, IsDir: isDir,
			})
		}
	}
	f.manifest = manifest
	cb := f.onRemote
	f.mu.Unlock()

	// A real server announces its clipboard asynchronously; the demo answers at
	// once so the round trip is visible.
	if cb != nil {
		cb(manifest)
	}
}

// OnRemoteFiles implements rdp.Files.
func (f *demoFiles) OnRemoteFiles(cb func([]rdp.RemoteFile)) error {
	f.mu.Lock()
	f.onRemote = cb
	manifest := append([]rdp.RemoteFile(nil), f.manifest...)
	f.mu.Unlock()
	if len(manifest) > 0 && cb != nil {
		cb(manifest)
	}
	return nil
}

// ReadRemoteFile implements rdp.Files by reading from the offered provider, the
// same read a real paste would perform.
func (f *demoFiles) ReadRemoteFile(index uint32, off int64, n int) ([]byte, error) {
	f.mu.Lock()
	provider := f.provider
	var entry rdp.RemoteFile
	found := false
	for _, e := range f.manifest {
		if e.Index == index {
			entry, found = e, true
			break
		}
	}
	f.mu.Unlock()

	if provider == nil || !found {
		return nil, fmt.Errorf("demo: no file at index %d", index)
	}
	if entry.IsDir {
		return nil, fmt.Errorf("demo: %q is a directory", entry.Name)
	}
	if n <= 0 {
		n = demoDownloadChunk
	}
	if off < 0 || (entry.Size > 0 && off >= entry.Size) {
		return nil, nil
	}
	if entry.Size > 0 && off+int64(n) > entry.Size {
		n = int(entry.Size - off)
	}

	rc, err := provider.Open(entry.Name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if off > 0 {
		if seeker, ok := rc.(io.Seeker); ok {
			if _, err := seeker.Seek(off, io.SeekStart); err != nil {
				return nil, err
			}
		} else if _, err := io.CopyN(io.Discard, rc, off); err != nil {
			return nil, err
		}
	}
	data := make([]byte, n)
	read, err := io.ReadFull(rc, data)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return data[:read], nil
}

// ClearRemoteFiles implements rdp.Files.
func (f *demoFiles) ClearRemoteFiles() {
	f.mu.Lock()
	f.manifest = nil
	cb := f.onRemote
	f.mu.Unlock()
	if cb != nil {
		cb(nil)
	}
}

const demoDownloadChunk = 1 << 20
