package session

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"sync"
	"time"

	"github.com/adfnekc/ad_explorer/internal/rdp"
	"github.com/adfnekc/ad_explorer/internal/stream"
	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// Quality defaults for a new session.
const (
	DefaultQualityMin   = 25
	DefaultQualityMax   = 90
	DefaultQualityStart = 70
)

// Timing defaults for a session's loops.
const (
	// DefaultStatsInterval is how often the HUD is refreshed.
	DefaultStatsInterval = time.Second
	// DefaultIdleCheckInterval is how often the idle deadline is tested. It
	// only has to be fine relative to the timeout, which is minutes.
	DefaultIdleCheckInterval = 15 * time.Second
	// DefaultFrameInterval is the shortest gap between two encoded frames, so
	// twenty a second. Encoding is the expensive part of a session — a full
	// 1280x720 frame costs about 32 ms — so a server that draws in a burst must
	// not become a burst of encodes. Regions that arrive inside one interval are
	// merged into the next frame.
	DefaultFrameInterval = 50 * time.Millisecond
)

// options is what a Session needs beyond the rdp.Config.
//
// The intervals live here rather than as package variables so a session's
// behaviour is fixed when it is built. Package-level mutable timing is how a
// test that shortens a ticker for itself corrupts a session running alongside
// it, which is a race the detector reports against production code.
type options struct {
	idleTimeout  time.Duration
	qualityMin   int
	qualityMax   int
	qualityStart int
	statsEvery   time.Duration
	idleEvery    time.Duration
	frameEvery   time.Duration
	// reconnectGrace is how long a detached session is kept. A negative value
	// disables it: the session ends as soon as the browser does.
	reconnectGrace time.Duration
	now            func() time.Time
	onDone         func(*Session)
}

func (o options) time() time.Time {
	if o.now == nil {
		return time.Now()
	}
	return o.now()
}

// remote is what a session needs from the far side.
//
// *rdp.Session satisfies it, and so does a recorder in the tests. That is what
// lets the whole input path, the event loop and the idle policy be exercised
// without a server, which is the part most likely to be wrong.
type remote interface {
	Key(code string, down bool) error
	KeyScancode(sc uint16, down bool)
	MouseMove(x, y int)
	MouseButton(button int, down bool, x, y int)
	MouseWheel(delta, x, y int)
	UnicodeKey(r rune) error
	SendCtrlAltEnd()
	SetClipboardText(text string) error
	RequestClipboardText() error
	Framebuffer() (rdp.Framebuffer, error)
	OnFrame(f func(dirty []image.Rectangle)) error
	OnCursor(f func(rdp.Cursor)) error
	RequestResize(w, h int) error
	OnResize(f func(w, h int)) error
	Files() rdp.Files
	Events() <-chan rdp.Event
	Close()
}

// Session is one live remote desktop with at most one browser attached.
type Session struct {
	id     string
	cfg    rdp.Config
	opts   options
	remote remote

	builder *stream.Builder
	acc     *stream.Accumulator
	quality *stream.Quality

	done      chan struct{}
	closeOnce sync.Once
	// firstAttach is closed by the first successful Attach. Until then there is
	// no event loop and so none of the timers that normally reap a session,
	// which is what the attach deadline waits on.
	firstAttach chan struct{}

	// writeMu serialises transport writes, so a Transport does not have to be
	// safe for concurrent use.
	writeMu sync.Mutex

	mu        sync.Mutex
	transport Transport
	source    FrameSource
	files     *fileTransfer
	attached  bool
	closed    bool
	// started is set by the first Attach: the event loop, the frame source and
	// the input observers run once and outlive any one browser, so a reconnect
	// must not start them a second time.
	started bool
	// epoch changes on every attach and detach. A grace timer records it and
	// does nothing if it has moved, which is what stops a timer left behind by
	// one disconnect from ending the session a reconnect recovered.
	epoch uint64
	// repaint requests a full frame. The accumulator belongs to the frame loop,
	// so a reconnect asks for the repaint here rather than seeding it directly.
	repaint   bool
	lastInput time.Time
	reported  map[string]bool
	startedAt time.Time

	statsMu   sync.Mutex
	statsFrom statPoint
	frames    uint64
	bytes     uint64
	dropped   uint64
}

// statPoint is a counter snapshot used to turn cumulative totals into rates.
type statPoint struct {
	at     time.Time
	frames uint64
	bytes  uint64
}

func newSession(id string, cfg rdp.Config, rs remote, opts options) *Session {
	if opts.qualityMin <= 0 {
		opts.qualityMin = DefaultQualityMin
	}
	if opts.qualityMax <= 0 {
		opts.qualityMax = DefaultQualityMax
	}
	if opts.qualityStart <= 0 {
		opts.qualityStart = DefaultQualityStart
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.statsEvery <= 0 {
		opts.statsEvery = DefaultStatsInterval
	}
	if opts.idleEvery <= 0 {
		opts.idleEvery = DefaultIdleCheckInterval
	}
	if opts.frameEvery <= 0 {
		opts.frameEvery = DefaultFrameInterval
	}
	// Zero is the default grace, which is what a caller that does not know about
	// reconnection gets; a negative value is the explicit "end with the browser".
	if opts.reconnectGrace == 0 {
		opts.reconnectGrace = DefaultReconnectGrace
	}
	started := opts.now()

	s := &Session{
		id:      id,
		cfg:     cfg,
		opts:    opts,
		remote:  rs,
		builder: stream.NewBuilder(nil),
		// Replaced once the frame source reports the real desktop size.
		acc:         stream.NewAccumulator(image.Rect(0, 0, cfg.Width, cfg.Height), stream.DefaultMaxRects, stream.DefaultFullFraction),
		quality:     stream.NewQuality(opts.qualityMin, opts.qualityMax, opts.qualityStart),
		done:        make(chan struct{}),
		firstAttach: make(chan struct{}),
		lastInput:   started,
		startedAt:   started,
		statsFrom:   statPoint{at: started},
	}

	// A session that is created and then never attached has no event loop, so
	// none of the timers that normally reap a session exist for it. A create
	// whose WebSocket upgrade never completes would otherwise hold a remote
	// desktop and a session slot until the process restarted.
	if opts.reconnectGrace >= 0 {
		go s.expireUnattached(opts.reconnectGrace)
	}
	return s
}

// expireUnattached ends a session the browser never came back to attach to.
func (s *Session) expireUnattached(grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-timer.C:
		s.mu.Lock()
		reap := !s.started && !s.closed
		s.mu.Unlock()
		if reap {
			s.shutdown(wsframe.CloseClientGone, nil)
		}
	case <-s.firstAttach:
	case <-s.done:
	}
}

// ID is the session's opaque identifier.
func (s *Session) ID() string { return s.id }

// Info describes the session for the API.
func (s *Session) Info() Info {
	s.mu.Lock()
	attached := s.attached
	s.mu.Unlock()
	return Info{
		ID:        s.id,
		Name:      s.cfg.Name,
		Host:      s.cfg.Host,
		Width:     s.cfg.Width,
		Height:    s.cfg.Height,
		StartedAt: s.startedAt,
		Attached:  attached,
	}
}

// Attach connects a browser and starts the loops that serve it.
//
// It reports ErrAlreadyAttached if another transport is already on this
// session: one session is one tab, because two would fight over the pointer and
// the clipboard. A session whose browser went away is detached rather than
// ended, so this is also how a reload or a reconnected socket gets the desktop
// back.
func (s *Session) Attach(t Transport) error {
	if t == nil {
		return ErrClosed
	}

	// The transport is published under the write lock so a frame from the
	// already running frame loop cannot overtake the header on a reconnect: the
	// client has no canvas to size until it has the header.
	s.writeMu.Lock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.writeMu.Unlock()
		return ErrClosed
	}
	if s.attached {
		s.mu.Unlock()
		s.writeMu.Unlock()
		return ErrAlreadyAttached
	}
	first := !s.started
	s.started = true
	if first {
		close(s.firstAttach)
	}
	s.attached = true
	s.transport = t
	s.epoch++
	if !first {
		// The frame loop is already running and the previous client's canvas is
		// gone, so the next frame it sends has to be the whole desktop.
		s.repaint = true
	}
	s.lastInput = s.opts.time()
	s.mu.Unlock()

	if first {
		// run sends the header before it starts the frame loop, so nothing can
		// overtake it on a first attach.
		s.writeMu.Unlock()
		go s.run(t)
		return nil
	}

	err := t.SendControl(s.header())
	s.writeMu.Unlock()
	if err != nil {
		// The socket died before it could be greeted. Treat it as a browser that
		// never arrived rather than a client in a session.
		s.detach(t)
		return nil
	}
	go s.readLoop(t)
	return nil
}

// Close ends the session and releases the remote connection.
func (s *Session) Close() { s.shutdown(wsframe.CloseUserRequested, nil) }

// header is the SessionReady a freshly attached client gets first.
func (s *Session) header() wsframe.SessionReady {
	s.mu.Lock()
	defer s.mu.Unlock()
	return wsframe.SessionReady{
		Width:    s.cfg.Width,
		Height:   s.cfg.Height,
		Name:     s.cfg.Name,
		Protocol: protocolName(s.cfg.Protocol),
	}
}

// run is the event loop for one session: everything that flows from the remote
// towards the client passes through here, on one goroutine, so the writes stay
// ordered. It starts once, on the first attach, and outlives individual
// browsers so a detached session can be re-attached.
func (s *Session) run(t Transport) {
	defer s.finish()

	if err := s.send(s.header()); err != nil {
		return
	}

	// If there is no pixel source the client is told once, up front, naming the
	// blocker: that is the difference between a UI that explains itself and one
	// that shows an empty canvas forever.
	s.startFrames()

	// A Close may have raced the header. startFrames has already released the
	// source it resolved in that case, and there is no event loop left to serve.
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}

	s.startCursor()
	s.startResize()
	s.startFiles()

	go s.readLoop(t)

	stats := time.NewTicker(s.opts.statsEvery)
	defer stats.Stop()
	idle := time.NewTicker(s.opts.idleEvery)
	defer idle.Stop()

	for {
		select {
		case ev, ok := <-s.remote.Events():
			if !ok {
				s.shutdown(wsframe.CloseRemoteEnded, nil)
				return
			}
			if !s.handleRemoteEvent(ev) {
				return
			}
		case <-stats.C:
			s.sendStats()
		case <-idle.C:
			if s.idleExpired() {
				s.shutdown(wsframe.CloseIdleTimeout, nil)
				return
			}
		case <-s.done:
			return
		}
	}
}

// readLoop turns control messages from one browser into remote input. It runs on
// its own goroutine because Receive blocks; everything it learns is handed to
// rdp, whose input methods are safe to call concurrently. A read loop is per
// attachment: one ends when its browser goes, and a reconnect starts another.
func (s *Session) readLoop(t Transport) {
	for {
		msg, err := t.Receive()
		if err != nil {
			// The browser is gone. The remote connection and the frame source
			// stay up for the reconnect grace so a reload can attach to the same
			// desktop instead of losing it.
			s.detach(t)
			return
		}

		switch m := msg.(type) {
		case ControlMessage:
			if !s.handleControl(m.Control) {
				return
			}
		case ChunkMessage:
			s.handleFileChunk(m.Token, m.Offset, m.Data)
		}
	}
}

// detach ends one browser's attachment without ending the session. The remote
// connection and the frame source keep running for the reconnect grace, so a
// reload or a network blip can attach to the same desktop.
//
// The transport is closed here, so the closing notice a grace expiry sends has
// nowhere to go. That is the honest outcome: the client the notice is for is the
// one that went away, and a client that is still there would have been
// re-attached before the grace ran out.
func (s *Session) detach(t Transport) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.transport != t {
		// A newer transport has already taken over; this read loop is stale.
		s.mu.Unlock()
		return
	}
	s.attached = false
	s.epoch++
	gen := s.epoch
	grace := s.opts.reconnectGrace
	s.mu.Unlock()

	_ = t.Close()

	if grace < 0 {
		// No grace: end as soon as the browser goes, which is what the gateway
		// did before reconnection existed.
		s.shutdown(wsframe.CloseUserRequested, nil)
		return
	}
	go s.expireDetached(gen, grace)
}

// expireDetached ends a session whose browser never came back. The generation
// makes the timer harmless after a reconnect: a session that recovered must not
// be ended by the timer left behind by the disconnect it recovered from.
func (s *Session) expireDetached(gen uint64, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-timer.C:
		s.mu.Lock()
		expired := !s.closed && !s.attached && s.epoch == gen
		s.mu.Unlock()
		if expired {
			s.shutdown(wsframe.CloseClientGone, nil)
		}
	case <-s.done:
	}
}

// attachState reports whether a browser is attached and consumes any pending
// repaint request. The two are read under one lock so that a reconnect landing
// between them cannot leave an incremental frame as the first thing a new client
// sees.
func (s *Session) attachState() (attached, repaint bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attached = s.attached
	repaint = s.repaint
	s.repaint = false
	return attached, repaint
}

// handleControl applies one client message. It reports false when the session
// should stop.
//
// The cases are pointers because that is what wsframe.DecodeControl returns: the
// decoder needs an address to unmarshal into. Matching on the value type
// compiles and then silently drops every message, which is a mistake this
// package made once already — the fake transport in the tests now goes through
// the real codec so it cannot come back.
func (s *Session) handleControl(c wsframe.Control) bool {
	if s.handleFileControl(c) {
		return true
	}
	switch m := c.(type) {
	case *wsframe.InputKey:
		s.touch()
		s.observe(func(o InputObserver) { o.ObserveKey(m.Code, m.Down) })
		if err := s.remote.Key(m.Code, m.Down); err != nil {
			// Worth telling the client about, once per key: otherwise the key
			// silently does nothing and looks like a broken remote.
			s.reportUnsupported("key:"+m.Code, "this build has no scancode for the key "+m.Code)
		}

	case *wsframe.InputMouse:
		s.touch()
		s.observe(func(o InputObserver) { o.ObserveMouse(m.Action, m.Button, m.X, m.Y) })
		switch m.Action {
		case wsframe.PointerMove:
			s.remote.MouseMove(m.X, m.Y)
		case wsframe.PointerDown:
			s.remote.MouseButton(m.Button, true, m.X, m.Y)
		case wsframe.PointerUp:
			s.remote.MouseButton(m.Button, false, m.X, m.Y)
		}

	case *wsframe.InputWheel:
		s.touch()
		s.observe(func(o InputObserver) { o.ObserveWheel(m.Delta, m.X, m.Y) })
		s.remote.MouseWheel(m.Delta, m.X, m.Y)

	case *wsframe.InputText:
		s.touch()
		s.observe(func(o InputObserver) { o.ObserveText(m.Text) })
		for _, r := range m.Text {
			if err := s.remote.UnicodeKey(r); err != nil {
				s.reportUnsupported("input.text",
					"character input is not available: the gateway needs a Unicode keyboard "+
						"event API from grdp (requirement C). Use the clipboard panel instead.")
				break
			}
		}

	case *wsframe.SessionCAD:
		s.touch()
		s.remote.SendCtrlAltEnd()

	case *wsframe.ClipboardSet:
		s.touch()
		if err := s.remote.SetClipboardText(m.Text); err != nil {
			s.send(wsframe.ErrorMsgFrom(wsframe.ErrCodeInternal, "clipboard: "+err.Error()))
		}

	case *wsframe.ClipboardRequest:
		if err := s.remote.RequestClipboardText(); err != nil {
			s.send(wsframe.ErrorMsgFrom(wsframe.ErrCodeInternal, "clipboard: "+err.Error()))
		}

	case *wsframe.SessionQuality:
		// A manual choice replaces the value; the governor keeps its bounds so
		// a link that cannot carry it can still back off.
		s.quality.Set(m.Level)

	case *wsframe.SessionResize:
		// The server rounds to a mode it has, so the reply is not this number;
		// onResize reports what actually took effect.
		if err := s.remote.RequestResize(m.Width, m.Height); err != nil {
			s.send(wsframe.ErrorMsgFrom(wsframe.ErrCodeUnsupported,
				"could not change the desktop size: "+err.Error()))
		}

	case *wsframe.Ping:
		s.send(wsframe.Pong{T: m.T})

	case *wsframe.SessionClose:
		s.shutdown(wsframe.CloseUserRequested, nil)
		return false
	}
	return true
}

// handleRemoteEvent forwards something the remote did to the browser. It
// reports false when the session should stop.
func (s *Session) handleRemoteEvent(ev rdp.Event) bool {
	switch e := ev.(type) {
	case rdp.Ready:
		// The client already has SessionReady from the start of run; this only
		// fires if grdp reports a size other than the one requested.
		if e.Width != s.cfg.Width || e.Height != s.cfg.Height {
			s.send(wsframe.SessionReady{
				Width:    e.Width,
				Height:   e.Height,
				Name:     s.cfg.Name,
				Protocol: protocolName(s.cfg.Protocol),
			})
		}

	case rdp.ClipboardText:
		// Delivered as a log entry, never written to the local clipboard: a
		// busy remote must not overwrite what the operator is doing.
		s.send(wsframe.ClipboardText{Text: e.Text, TS: e.At.UnixMilli()})

	case rdp.CursorPos:
		s.send(wsframe.CursorPos{X: e.X, Y: e.Y})

	case rdp.ErrorEvent:
		s.send(wsframe.ErrorMsgFrom(e.Code, e.Err.Error()))

	case rdp.Closed:
		s.shutdown(closeReasonFor(e.Reason), e.Err)
		return false
	}
	return true
}

// send writes one control message, ending the session if the transport is gone.
// With no transport — a session that is detached or already closing — the
// message is dropped: there is nowhere to write it, and that is not a failure
// the callers can act on.
func (s *Session) send(c wsframe.Control) error {
	s.writeMu.Lock()
	t := s.transportLocked()
	if t == nil {
		s.writeMu.Unlock()
		return nil
	}
	err := t.SendControl(c)
	s.writeMu.Unlock()
	if err != nil {
		// A write that fails means this browser is gone, not that the session
		// should end: the remote desktop is still there and the grace exists so
		// a reconnect can reach it. Ending the session here would make the
		// grace useless exactly when it is needed, because a silent drop is
		// noticed by the next frame's write long before the read deadline.
		s.detach(t)
	}
	return err
}

// sendFrames writes encoded frames, ending the session if the transport is gone.
// While the session is detached the frames are dropped: there is no browser to
// receive them, but that is not a reason to end the session or to stop the
// frame loop, so a reconnect still finds the desktop.
func (s *Session) sendFrames(frames []wsframe.Frame) error {
	if len(frames) == 0 {
		return nil
	}
	s.writeMu.Lock()
	t := s.transportLocked()
	if t == nil {
		s.writeMu.Unlock()
		return nil
	}
	err := t.SendFrames(frames)
	s.writeMu.Unlock()

	if err != nil {
		// As above: the socket is gone, the session is not.
		s.detach(t)
		return err
	}
	s.recordSent(frames)
	return nil
}

// transportLocked returns the transport a write may go to, or nil while the
// session is detached or closing. It is nil rather than the stale transport so
// a frame produced during the grace is dropped instead of being written to a
// socket that is already gone.
func (s *Session) transportLocked() Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.attached {
		return nil
	}
	return s.transport
}

// startFrames subscribes to the desktop and begins encoding it.
//
// It reports whether a picture is available. When grdp requirement A lands the
// source comes from the remote and this code path is unchanged; when the
// gateway is started with -demo it comes from a synthetic desktop.
func (s *Session) startFrames() {
	src, err := s.frameSource()
	if err != nil {
		s.reportUnsupported("frames",
			"the desktop picture is not available yet: the gateway needs an always-on "+
				"composited framebuffer from grdp (requirement A). Input and clipboard work.")
		return
	}

	s.mu.Lock()
	if s.closed {
		// Close won the race. The source is this session's to release, and
		// shutdown has already looked and found nothing, so it happens here.
		s.mu.Unlock()
		closeFrameSource(src)
		return
	}
	s.source = src
	// The accumulator is sized from the source, not from the requested size:
	// a server is free to hand back a different desktop, and the full-frame
	// threshold and the clipping both depend on knowing which one is real.
	s.acc = stream.NewAccumulator(src.Bounds(), stream.DefaultMaxRects, stream.DefaultFullFraction)
	// The client starts with an empty canvas, so the first thing it receives
	// has to be the whole desktop. Seeding the accumulator with the full bounds
	// promotes the first frame to a repaint; without this the client draws every
	// changed region onto nothing and the desktop never appears.
	s.acc.Add(src.Bounds())
	s.mu.Unlock()

	go s.frameLoop(src)
}

// closeFrameSource releases a source if it holds anything to release.
func closeFrameSource(src FrameSource) {
	if closer, ok := src.(io.Closer); ok {
		_ = closer.Close()
	}
}

// frameSource resolves the pixels: a remote that can supply them, a synthetic
// desktop in demo mode, or the grdp adapter that is still waiting on
// requirement A.
func (s *Session) frameSource() (FrameSource, error) {
	if provider, ok := s.remote.(frameSourceProvider); ok {
		return provider.FrameSource()
	}
	return grdpFrames(s.remote)
}

// frameLoop turns changed regions into encoded frames, at a bounded rate.
//
// Encoding is the expensive part of a session, so this does not encode once per
// update from the server: it holds the newest frame and encodes it on a tick,
// with every region that arrived in between merged into it. A server that draws
// sixty times a second therefore costs twenty encodes, not sixty.
//
// Holding the newest frame and dropping the one before it is safe because a
// published image is the whole desktop as of that moment, so a newer one
// supersedes an older one's pixels. It does not supersede its *news*, which is
// why every region is added to the accumulator before the frame that carried it
// is released: a client that was never told about a region would keep showing
// what used to be there.
//
// It runs on its own goroutine so that a slow client slows this loop rather than
// the input path.
func (s *Session) frameLoop(src FrameSource) {
	ticker := time.NewTicker(s.opts.frameEvery)
	defer ticker.Stop()

	// held is the newest desktop the source has published, and it is kept after
	// it has been encoded rather than released. That is what makes a re-attach
	// able to repaint: a client that reloaded onto a desktop nothing is drawing
	// on would otherwise wait for the remote to change something before it saw
	// anything at all. The pool is one larger than the update buffer so holding
	// this one cannot starve the source.
	var held *Update
	defer func() {
		if held != nil {
			held.Release()
		}
	}()

	for {
		select {
		case update, ok := <-src.Updates():
			if !ok {
				return
			}
			for _, r := range update.Regions {
				s.acc.Add(r)
			}
			if held != nil {
				// Superseded: its pixels are in the newer image, and its
				// regions are already in the accumulator.
				held.Release()
			}
			h := update
			held = &h

		case <-ticker.C:
			attached, repaint := s.attachState()
			if repaint && held != nil {
				// A re-attached client holds nothing, so its first frame has to
				// be the whole desktop. Seeding the accumulator with its bounds
				// is what turns the next frame into a full repaint — the same
				// thing startFrames does for the first client — because the
				// promotion lives in the accumulator and not in the encoder.
				// Without it the next update would go out as a rectangle drawn
				// onto a canvas the client no longer has.
				s.acc.Add(held.Image.Bounds())
			}
			if held == nil || s.acc.Empty() {
				continue
			}
			if !attached {
				// Nobody is attached to receive a frame, so there is nowhere to
				// send it. The regions stay accumulated, so the client that
				// attaches next is handed the desktop as of now rather than a
				// rectangle drawn onto nothing.
				continue
			}
			if !s.render(*held) {
				return
			}

		case <-s.done:
			return
		}
	}
}

// render encodes what has accumulated and sends it. It reports false when the
// session should stop.
func (s *Session) render(update Update) bool {
	img := update.Image
	// A resize replaces the buffer and every coordinate that referred to it, so
	// the accumulator is rebuilt and the whole of the new desktop is marked
	// dirty. It is the image being encoded that decides, not the source: a held
	// frame may predate a resize the source has already taken.
	if b := img.Bounds(); b != s.acc.Bounds() {
		s.acc = stream.NewAccumulator(b, stream.DefaultMaxRects, stream.DefaultFullFraction)
		s.acc.Add(b)
	}

	if img == nil {
		// The source has nothing to show yet. Dropping the regions would lose
		// the update, so they stay accumulated until the next one arrives.
		return true
	}

	frames, err := s.builder.Build(img, s.acc, s.quality.Value())
	s.acc.Reset()
	if err != nil {
		s.noteDropped()
		return true
	}
	return s.sendFrames(frames) == nil
}

// noteDropped records a frame that never went out.
func (s *Session) noteDropped() {
	s.statsMu.Lock()
	s.dropped++
	s.statsMu.Unlock()
	s.quality.OnDropped()
}

// observe forwards an input event to the frame source, when the source draws
// what the session sends. The demo desktop does; a real one does not.
func (s *Session) observe(f func(InputObserver)) {
	s.mu.Lock()
	src := s.source
	s.mu.Unlock()
	if observer, ok := src.(InputObserver); ok {
		f(observer)
	}
}

// startCursor subscribes to pointer shape changes.
//
// The pointer is not part of the desktop image: it sits over it and the server
// reports its shape separately. A client that is never told about it keeps
// drawing whatever it drew first, which is how a pointer ends up looking like an
// hourglass forever.
func (s *Session) startCursor() {
	if err := s.remote.OnCursor(s.onCursor); err != nil {
		s.reportUnsupported("cursor",
			"pointer shapes are unavailable, so the local pointer is shown instead: "+err.Error())
	}
}

// onCursor forwards a shape to the client, as pixels or as a name to look up.
func (s *Session) onCursor(cur rdp.Cursor) {
	if cur.Hidden() {
		s.send(wsframe.CursorSystem{Hidden: true})
		return
	}
	if cur.System || cur.Image == nil {
		// No pixels: the client substitutes its own artwork for this pointer.
		s.send(wsframe.CursorSystem{SystemType: cur.SystemType})
		return
	}

	payload, err := encodeCursorPNG(cur.Image)
	if err != nil {
		// A pointer is not worth failing a session over; the client keeps the
		// shape it has.
		return
	}
	b := cur.Image.Bounds()
	s.sendFrames([]wsframe.Frame{{
		Type:   wsframe.TypeCursor,
		Format: wsframe.FormatPNG,
		// The header carries the hotspot in the origin and the shape size in
		// the extent, which is all the client needs to place it.
		Rect:    image.Rect(cur.Hotspot.X, cur.Hotspot.Y, cur.Hotspot.X+b.Dx(), cur.Hotspot.Y+b.Dy()),
		Payload: payload,
	}})
}

// startResize subscribes to the desktop size actually in effect.
//
// The server rounds a requested size to a mode it has, so the size that takes
// effect is the one reported here rather than the one that was asked for, and
// the client has to be told which it got.
func (s *Session) startResize() {
	if err := s.remote.OnResize(s.onResize); err != nil {
		// Not fatal: the client is told when it asks for a size instead.
		return
	}
}

func (s *Session) onResize(w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	s.mu.Lock()
	s.cfg.Width, s.cfg.Height = w, h
	name, protocol := s.cfg.Name, protocolName(s.cfg.Protocol)
	s.mu.Unlock()

	s.send(wsframe.SessionReady{Width: w, Height: h, Name: name, Protocol: protocol})
}

// encodeCursorPNG turns a decoded pointer into the bytes of the cursor frame.
// PNG because the shape needs its alpha; a cursor is tiny, so the format costs
// nothing worth measuring.
func encodeCursorPNG(img *image.RGBA) ([]byte, error) {
	if img == nil || img.Bounds().Empty() {
		return nil, rdp.ErrNotReady
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// reportUnsupported sends an error for a feature that cannot work, at most once
// per key. Repeating it on every keystroke would drown the client.
func (s *Session) reportUnsupported(key, detail string) {
	s.mu.Lock()
	if s.reported == nil {
		s.reported = make(map[string]bool)
	}
	if s.reported[key] {
		s.mu.Unlock()
		return
	}
	s.reported[key] = true
	s.mu.Unlock()

	s.send(wsframe.ErrorMsgFrom(wsframe.ErrCodeUnsupported, detail))
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastInput = s.opts.time()
	s.mu.Unlock()
}

func (s *Session) idleExpired() bool {
	if s.opts.idleTimeout <= 0 {
		return false
	}
	s.mu.Lock()
	last := s.lastInput
	s.mu.Unlock()
	return s.opts.time().Sub(last) > s.opts.idleTimeout
}

// sendStats turns the cumulative counters into the rates the HUD shows.
func (s *Session) sendStats() {
	now := s.opts.time()

	s.statsMu.Lock()
	elapsed := now.Sub(s.statsFrom.at).Seconds()
	deltaFrames := s.frames - s.statsFrom.frames
	deltaBytes := s.bytes - s.statsFrom.bytes
	s.statsFrom = statPoint{at: now, frames: s.frames, bytes: s.bytes}
	dropped := s.dropped
	s.statsMu.Unlock()

	stats := wsframe.SessionStats{Quality: s.quality.Value(), Dropped: int(dropped)}
	if elapsed > 0 {
		stats.FPS = float64(deltaFrames) / elapsed
		stats.KBps = float64(deltaBytes) / 1024 / elapsed
	}
	s.send(stats)
}

func (s *Session) recordSent(frames []wsframe.Frame) {
	s.statsMu.Lock()
	s.frames += uint64(len(frames))
	for _, f := range frames {
		s.bytes += uint64(len(f.Payload))
	}
	s.statsMu.Unlock()

	for range frames {
		s.quality.OnSent()
	}
}

// shutdown ends the session once, from whichever goroutine notices first.
//
// Everything that has to happen exactly once lives here rather than in finish,
// because a session that is created and never attached never runs its event
// loop — and without this it would sit in the registry forever, holding a
// remote connection nobody can reach.
func (s *Session) shutdown(reason string, cause error) {
	s.closeOnce.Do(func() {
		close(s.done)

		// Both fields are read under the lock: startFrames publishes the source
		// from the event loop, which is still running while this may be called
		// from the read loop.
		s.mu.Lock()
		t := s.transport
		src := s.source
		s.closed = true
		s.attached = false
		s.mu.Unlock()

		if t != nil {
			s.writeMu.Lock()
			msg := wsframe.SessionClosed{Reason: reason}
			if cause != nil {
				msg.Message = cause.Error()
			}
			_ = t.SendControl(msg)
			_ = t.Close()
			s.writeMu.Unlock()
		}

		closeFrameSource(src)
		s.remote.Close()
		if s.opts.onDone != nil {
			s.opts.onDone(s)
		}
	})
}

// finish runs when the event loop returns, whatever ended it. shutdown has
// already done the work; this only guarantees it happened for a loop that
// exited through a path that did not call it.
func (s *Session) finish() {
	s.shutdown(wsframe.CloseRemoteEnded, nil)
}

// protocolName normalises the empty protocol to the name it actually means.
func protocolName(p string) string {
	if p == "" {
		return "tls"
	}
	return p
}

// closeReasonFor maps the reason a remote session ended to the one the client
// understands.
func closeReasonFor(reason string) string {
	switch reason {
	case rdp.ReasonUserRequested:
		return wsframe.CloseUserRequested
	case rdp.ReasonServerShutdown:
		return wsframe.CloseServerShutdown
	case rdp.ReasonError:
		return wsframe.CloseError
	default:
		return wsframe.CloseRemoteEnded
	}
}
