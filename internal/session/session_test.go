package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/png"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adfnekc/ad_explorer/internal/rdp"
	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// ---------------------------------------------------------------- harness

// fakeRemote stands in for a grdp session. It records what the session asked it
// to do and lets a test push events the other way.
type fakeRemote struct {
	mu     sync.Mutex
	calls  []string
	events chan rdp.Event
	closed bool

	src            FrameSource
	files          rdp.Files
	framebufferErr error
	onFrameErr     error
	onCursorErr    error
	resizeErr      error
	cursorSink     func(rdp.Cursor)
	resizeSink     func(w, h int)
	unicodeErr     error
	clipboardErr   error
	keyErr         error
	unmappedKeys   map[string]bool
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{
		events:         make(chan rdp.Event, 32),
		framebufferErr: fmt.Errorf("%w: requirement A", rdp.ErrGRDPPending),
		onFrameErr:     fmt.Errorf("%w: requirement A", rdp.ErrGRDPPending),
		onCursorErr:    fmt.Errorf("%w: requirement B", rdp.ErrGRDPPending),
		unicodeErr:     fmt.Errorf("%w: requirement C", rdp.ErrGRDPPending),
		unmappedKeys:   map[string]bool{"Pause": true},
	}
}

func (f *fakeRemote) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeRemote) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRemote) Key(code string, down bool) error {
	if f.unmappedKeys[code] {
		return fmt.Errorf("%w: %q", rdp.ErrUnknownKey, code)
	}
	dir := "up"
	if down {
		dir = "down"
	}
	f.record("key %s %s", code, dir)
	return nil
}

func (f *fakeRemote) KeyScancode(sc uint16, down bool) { f.record("scancode %#x %v", sc, down) }

func (f *fakeRemote) MouseMove(x, y int) { f.record("move %d %d", x, y) }

func (f *fakeRemote) MouseButton(button int, down bool, x, y int) {
	f.record("button %d %v %d %d", button, down, x, y)
}

func (f *fakeRemote) MouseWheel(delta, x, y int) { f.record("wheel %d %d %d", delta, x, y) }

func (f *fakeRemote) UnicodeKey(r rune) error {
	if f.unicodeErr != nil {
		return f.unicodeErr
	}
	f.record("unicode %q", r)
	return nil
}

func (f *fakeRemote) SendCtrlAltEnd() { f.record("cad") }

func (f *fakeRemote) SetClipboardText(text string) error {
	if f.clipboardErr != nil {
		return f.clipboardErr
	}
	f.record("clipboard.set %q", text)
	return nil
}

func (f *fakeRemote) RequestClipboardText() error {
	f.record("clipboard.request")
	return nil
}

// FrameSource implements frameSourceProvider, which is how a remote that has
// pixels hands them over. A nil source falls back to the grdp surface below.
func (f *fakeRemote) FrameSource() (FrameSource, error) {
	if f.src == nil {
		return nil, fmt.Errorf("%w: no source configured", rdp.ErrGRDPPending)
	}
	return f.src, nil
}

func (f *fakeRemote) Framebuffer() (rdp.Framebuffer, error) { return nil, f.framebufferErr }

// Files implements remote. A nil return is "this transport has no file
// transfer", which is what most of the tests want; a test that cares sets one.
func (f *fakeRemote) Files() rdp.Files { return f.files }

func (f *fakeRemote) OnFrame(func([]image.Rectangle)) error { return f.onFrameErr }

func (f *fakeRemote) OnCursor(fn func(rdp.Cursor)) error {
	f.mu.Lock()
	f.cursorSink = fn
	f.mu.Unlock()
	return f.onCursorErr
}

// pushCursor delivers a shape to whoever subscribed, the way grdp's callback
// would.
func (f *fakeRemote) pushCursor(cur rdp.Cursor) {
	f.mu.Lock()
	sink := f.cursorSink
	f.mu.Unlock()
	if sink != nil {
		sink(cur)
	}
}

func (f *fakeRemote) RequestResize(w, h int) error {
	if f.resizeErr != nil {
		return f.resizeErr
	}
	f.record("resize %dx%d", w, h)
	return nil
}

func (f *fakeRemote) OnResize(fn func(w, h int)) error {
	f.mu.Lock()
	f.resizeSink = fn
	f.mu.Unlock()
	return nil
}

// pushResize reports a size the way the server would after a request.
func (f *fakeRemote) pushResize(w, h int) {
	f.mu.Lock()
	sink := f.resizeSink
	f.mu.Unlock()
	if sink != nil {
		sink(w, h)
	}
}

func (f *fakeRemote) Events() <-chan rdp.Event { return f.events }

func (f *fakeRemote) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *fakeRemote) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakeTransport collects what the gateway sends and lets a test feed it client
// messages.
type fakeTransport struct {
	mu       sync.Mutex
	controls []wsframe.Control
	frames   [][]wsframe.Frame
	closed   bool
	sendErr  error

	// incoming carries encoded bytes rather than control values, so the fake
	// exercises the real codec. Passing decoded values straight in would hide a
	// mismatch between what the wire produces and what the session matches on,
	// which is exactly the bug this fake had to be changed to catch.
	incoming chan []byte
	chunks   chan ChunkMessage
	done     chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		incoming: make(chan []byte, 32),
		chunks:   make(chan ChunkMessage, 32),
		done:     make(chan struct{}),
	}
}

func (f *fakeTransport) SendControl(c wsframe.Control) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.controls = append(f.controls, c)
	return nil
}

func (f *fakeTransport) SendFrames(fr []wsframe.Frame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.frames = append(f.frames, fr)
	return nil
}

func (f *fakeTransport) Receive() (Message, error) {
	select {
	case b := <-f.incoming:
		c, err := wsframe.DecodeControl(b)
		if err != nil {
			return nil, err
		}
		return ControlMessage{Control: c}, nil
	case chunk := <-f.chunks:
		return chunk, nil
	case <-f.done:
		return nil, io.EOF
	}
}

// setSendErr makes every later write fail, which is how a silent drop shows up:
// the browser is gone but the socket has not said so.
func (f *fakeTransport) setSendErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

// sendChunk delivers a file range the way the real transport does.
func (f *fakeTransport) sendChunk(token, offset uint32, data []byte) {
	f.chunks <- ChunkMessage{Token: token, Offset: offset, Data: data}
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	already := f.closed
	f.closed = true
	f.mu.Unlock()
	if !already {
		close(f.done)
	}
	return nil
}

func (f *fakeTransport) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// send queues a client message the way the network would deliver it: encoded,
// then decoded by ReceiveControl.
func (f *fakeTransport) send(t *testing.T, c wsframe.Control) {
	t.Helper()
	b, err := wsframe.EncodeControl(c)
	if err != nil {
		t.Fatalf("EncodeControl(%T): %v", c, err)
	}
	f.incoming <- b
}

// firstOfType returns the first control of a type, or nil.
func (f *fakeTransport) firstOfType(kind string) wsframe.Control {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.controls {
		if c.ControlType() == kind {
			return c
		}
	}
	return nil
}

func (f *fakeTransport) countOfType(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.controls {
		if c.ControlType() == kind {
			n++
		}
	}
	return n
}

func (f *fakeTransport) controlTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.controls))
	for _, c := range f.controls {
		out = append(out, c.ControlType())
	}
	return out
}

// testClock is a clock the test advances from its own goroutine while the
// session's event loop reads it, so the value has to be synchronised. A plain
// captured variable here is a data race, which is how this type came to exist.
type testClock struct{ nanos atomic.Int64 }

func newTestClock(t time.Time) *testClock {
	c := &testClock{}
	c.nanos.Store(t.UnixNano())
	return c
}

func (c *testClock) Now() time.Time { return time.Unix(0, c.nanos.Load()).UTC() }

func (c *testClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

// allControls returns a copy of everything sent so far.
//
// Reading the field directly would race the event loop's appends, which is what
// -race caught here.
func (f *fakeTransport) allControls() []wsframe.Control {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wsframe.Control(nil), f.controls...)
}

// errorCodes lists the code of every ErrorMsg sent, in order.
func (f *fakeTransport) errorCodes() []string {
	var out []string
	for _, c := range f.allControls() {
		if e, ok := c.(wsframe.ErrorMsg); ok {
			out = append(out, e.Code)
		}
	}
	return out
}

// hasErrorWithCode reports whether an ErrorMsg carrying code was sent.
func (f *fakeTransport) hasErrorWithCode(code string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.controls {
		if e, ok := c.(wsframe.ErrorMsg); ok && e.Code == code {
			return true
		}
	}
	return false
}

// messagesMentioning collects the text of every ErrorMsg that mentions needle.
func (f *fakeTransport) messagesMentioning(needle string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.controls {
		if e, ok := c.(wsframe.ErrorMsg); ok && strings.Contains(e.Message, needle) {
			out = append(out, e.Message)
		}
	}
	return out
}

// drainStartup waits until the capability probes have been queued, so a test
// can snapshot counters without racing the event loop.
func drainStartup(t *testing.T, tr *fakeTransport) {
	t.Helper()
	tr.send(t, wsframe.Ping{T: -1})
	waitFor(t, "the startup burst", func() bool { return tr.firstOfType("pong") != nil })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// newTestSession builds a session around a fake remote with a controllable
// clock and short timers.
func newTestSession(t *testing.T, cfg rdp.Config) (*Session, *fakeRemote, *fakeTransport) {
	t.Helper()
	remote := newFakeRemote()
	s := newSession("test-id", cfg, remote, options{
		idleTimeout: time.Minute,
		statsEvery:  10 * time.Millisecond,
		idleEvery:   10 * time.Millisecond,
		frameEvery:  2 * time.Millisecond,
		now:         time.Now,
	})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		s.finish()
	})
	return s, remote, tr
}

func testConfig() rdp.Config {
	return rdp.Config{
		Host: "10.0.0.1:3389", Name: "web-01",
		Protocol: "nla", Width: 1280, Height: 720,
	}
}

// ---------------------------------------------------------------- header

func TestSessionSendsReadyFirst(t *testing.T) {
	_, _, tr := newTestSession(t, testConfig())

	waitFor(t, "SessionReady", func() bool { return tr.firstOfType("session.ready") != nil })

	ready, ok := tr.firstOfType("session.ready").(wsframe.SessionReady)
	if !ok {
		t.Fatalf("first control is not a SessionReady: %v", tr.controlTypes())
	}
	if ready.Width != 1280 || ready.Height != 720 {
		t.Errorf("ready size = %dx%d, want 1280x720", ready.Width, ready.Height)
	}
	if ready.Name != "web-01" || ready.Protocol != "nla" {
		t.Errorf("ready = %+v, want the configured name and protocol", ready)
	}

	// It must be the first message: the client has nothing to size its canvas
	// with until it arrives.
	types := tr.controlTypes()
	if len(types) == 0 || types[0] != "session.ready" {
		t.Errorf("message order starts with %v, want session.ready first", types)
	}
}

func TestSessionReportsEachPendingCapabilityOnce(t *testing.T) {
	// The framebuffer (A) and the pointer shape (B) are probed at startup so
	// the UI can explain the gap immediately. The Unicode keyboard (C) has no
	// probe: it is reported the first time a character actually needs it, which
	// TestSessionReportsUnicodeInputUnavailableOnce covers.
	_, _, tr := newTestSession(t, testConfig())
	drainStartup(t, tr)

	for _, want := range []string{"requirement A", "requirement B"} {
		if got := tr.messagesMentioning(want); len(got) != 1 {
			t.Errorf("got %d reports naming %s, want 1: %v", len(got), want, got)
		}
	}

	// A probe must not repeat itself: the event loop cannot spam this.
	before := len(tr.messagesMentioning("requirement A"))
	time.Sleep(50 * time.Millisecond)
	if after := len(tr.messagesMentioning("requirement A")); after != before {
		t.Errorf("the framebuffer gap was reported %d times, want %d", after, before)
	}

	for _, code := range tr.errorCodes() {
		if code != wsframe.ErrCodeUnsupported {
			t.Errorf("a capability probe used code %q, want %q", code, wsframe.ErrCodeUnsupported)
		}
	}
}

func TestSessionAttachIsExclusive(t *testing.T) {
	remote := newFakeRemote()
	s := newSession("id", testConfig(), remote, options{})
	t.Cleanup(func() { s.Close(); s.finish() })

	if err := s.Attach(newFakeTransport()); err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	if err := s.Attach(newFakeTransport()); !errors.Is(err, ErrAlreadyAttached) {
		t.Errorf("second Attach = %v, want ErrAlreadyAttached", err)
	}
}

// ---------------------------------------------------------------- input

func TestSessionRoutesInput(t *testing.T) {
	_, remote, tr := newTestSession(t, testConfig())

	tr.send(t, wsframe.InputKey{Code: "KeyA", Down: true})
	tr.send(t, wsframe.InputKey{Code: "KeyA", Down: false})
	tr.send(t, wsframe.InputMouse{X: 100, Y: 200, Action: wsframe.PointerMove})
	tr.send(t, wsframe.InputMouse{X: 100, Y: 200, Action: wsframe.PointerDown, Button: 0})
	tr.send(t, wsframe.InputMouse{X: 100, Y: 200, Action: wsframe.PointerUp, Button: 0})
	tr.send(t, wsframe.InputMouse{X: 5, Y: 6, Action: wsframe.PointerDown, Button: 2})
	tr.send(t, wsframe.InputWheel{X: 7, Y: 8, Delta: -3})
	tr.send(t, wsframe.SessionCAD{})

	want := []string{
		"key KeyA down",
		"key KeyA up",
		"move 100 200",
		"button 0 true 100 200",
		"button 0 false 100 200",
		"button 2 true 5 6",
		"wheel -3 7 8",
		"cad",
	}
	waitFor(t, "all input to arrive", func() bool { return len(remote.Calls()) >= len(want) })

	got := remote.Calls()
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSessionReportsAnUnmappedKeyOnce(t *testing.T) {
	_, remote, tr := newTestSession(t, testConfig())
	drainStartup(t, tr)
	before := tr.countOfType("error")

	// Pause has no scancode in this build. Pressing it repeatedly must produce
	// one explanation, not one per keystroke.
	for i := 0; i < 5; i++ {
		tr.send(t, wsframe.InputKey{Code: "Pause", Down: i%2 == 0})
	}
	tr.send(t, wsframe.Ping{T: 1})
	waitFor(t, "the queue to drain", func() bool { return tr.countOfType("pong") >= 2 })

	after := tr.countOfType("error")
	if after-before != 1 {
		t.Errorf("five presses of an unmapped key produced %d reports, want 1", after-before)
	}
	if len(remote.Calls()) != 0 {
		t.Errorf("an unmapped key reached the remote: %v", remote.Calls())
	}

	// A different unmapped key is a different gap and does get its own report.
	remote.unmappedKeys["Unidentified"] = true
	tr.send(t, wsframe.InputKey{Code: "Unidentified", Down: true})
	tr.send(t, wsframe.Ping{T: 2})
	waitFor(t, "the second key's report", func() bool { return tr.countOfType("error") > after })
}

func TestSessionReportsUnicodeInputUnavailableOnce(t *testing.T) {
	_, remote, tr := newTestSession(t, testConfig())
	drainStartup(t, tr)

	// The whole string must not produce one error per character, and nothing
	// may reach the remote: grdp has no Unicode keyboard API yet.
	tr.send(t, wsframe.InputText{Text: "中文测试"})
	tr.send(t, wsframe.Ping{T: 1})
	waitFor(t, "the second pong", func() bool { return tr.countOfType("pong") >= 2 })

	reports := tr.messagesMentioning("requirement C")
	if len(reports) != 1 {
		t.Errorf("got %d reports naming requirement C, want 1: %v", len(reports), reports)
	}
	if len(remote.Calls()) != 0 {
		t.Errorf("unicode input reached the remote despite the pending API: %v", remote.Calls())
	}

	// A second attempt adds nothing: the operator has been told.
	tr.send(t, wsframe.InputText{Text: "再试"})
	tr.send(t, wsframe.Ping{T: 2})
	waitFor(t, "the third pong", func() bool { return tr.countOfType("pong") >= 3 })
	if got := len(tr.messagesMentioning("requirement C")); got != 1 {
		t.Errorf("the unicode gap was reported %d times, want 1", got)
	}
}

func TestSessionRoutesClipboard(t *testing.T) {
	_, remote, tr := newTestSession(t, testConfig())

	tr.send(t, wsframe.ClipboardSet{Text: "hello 中文"})
	tr.send(t, wsframe.ClipboardRequest{})

	waitFor(t, "clipboard traffic", func() bool { return len(remote.Calls()) >= 2 })
	got := remote.Calls()
	if got[0] != `clipboard.set "hello 中文"` || got[1] != "clipboard.request" {
		t.Errorf("calls = %v", got)
	}
}

func TestSessionQualityControlChangesTheGovernor(t *testing.T) {
	s, _, tr := newTestSession(t, testConfig())

	// Inside the governor's range: a manual value below the floor is clamped to
	// it rather than honoured, and that is deliberate, so asking for one here
	// would test the clamp rather than the routing.
	const manual = DefaultQualityMin + 10
	tr.send(t, wsframe.SessionQuality{Level: manual})
	waitFor(t, "the quality to change", func() bool { return s.quality.Value() == manual })

	// The manual value survives but the governor keeps working around it.
	if s.quality.Value() != manual {
		t.Errorf("quality = %d, want %d", s.quality.Value(), manual)
	}
}

func TestSessionResizeReachesTheRemote(t *testing.T) {
	_, remote, tr := newTestSession(t, testConfig())
	drainStartup(t, tr)

	before := len(tr.errorCodes())
	tr.send(t, wsframe.SessionResize{Width: 1920, Height: 1080})
	waitFor(t, "the resize to reach the remote", func() bool {
		return strings.Contains(strings.Join(remote.Calls(), " "), "resize 1920x1080")
	})

	// A working resize must not add a complaint. The startup capability reports
	// are still there and are a different matter.
	if after := len(tr.errorCodes()); after != before {
		t.Errorf("a working resize added %d error reports: %v", after-before, tr.errorCodes())
	}
}

func TestSessionReportsTheSizeTheServerActuallyChose(t *testing.T) {
	// The server rounds to a mode it has, so the size that takes effect is the
	// one it reports. A client that kept the number it asked for would frame
	// the desktop wrongly.
	_, remote, tr := newTestSession(t, testConfig())
	drainStartup(t, tr)

	before := tr.countOfType("session.ready")
	tr.send(t, wsframe.SessionResize{Width: 1920, Height: 720})

	waitFor(t, "the request", func() bool {
		return strings.Contains(strings.Join(remote.Calls(), " "), "resize 1920x720")
	})
	remote.pushResize(1920, 768) // the server's answer, not the request

	waitFor(t, "a second SessionReady", func() bool {
		return tr.countOfType("session.ready") > before
	})

	ready, _ := tr.lastOfType("session.ready").(wsframe.SessionReady)
	if ready.Width != 1920 || ready.Height != 768 {
		t.Errorf("ready reports %dx%d, want the size the server chose", ready.Width, ready.Height)
	}
}

func TestSessionResizeFailureIsReported(t *testing.T) {
	s, remote, tr := newTestSession(t, testConfig())
	drainStartup(t, tr)
	remote.resizeErr = errors.New("this server has no Display Control channel")

	tr.send(t, wsframe.SessionResize{Width: 1920, Height: 1080})
	waitFor(t, "the failure to be reported", func() bool {
		return len(tr.messagesMentioning("Display Control")) > 0
	})
	_ = s
}

func TestSessionForwardsCursorShapes(t *testing.T) {
	_, remote, tr := newTestSession(t, testConfig())
	drainStartup(t, tr)

	// A decoded shape goes out as a PNG frame carrying its hotspot.
	img := image.NewRGBA(image.Rect(0, 0, 16, 20))
	remote.pushCursor(rdp.Cursor{Image: img, Hotspot: image.Pt(3, 4)})

	waitFor(t, "the cursor frame", func() bool {
		for _, f := range tr.allFrames(t) {
			if f.Type == wsframe.TypeCursor {
				return true
			}
		}
		return false
	})

	var cursor wsframe.Frame
	for _, f := range tr.allFrames(t) {
		if f.Type == wsframe.TypeCursor {
			cursor = f
		}
	}
	if cursor.Format != wsframe.FormatPNG {
		t.Errorf("cursor format = %#x, want PNG: the shape needs its alpha", cursor.Format)
	}
	if cursor.Rect.Min != image.Pt(3, 4) {
		t.Errorf("hotspot = %v, want (3,4) in the frame origin", cursor.Rect.Min)
	}
	if cursor.Rect.Dx() != 16 || cursor.Rect.Dy() != 20 {
		t.Errorf("shape size = %dx%d, want 16x20 in the frame extent", cursor.Rect.Dx(), cursor.Rect.Dy())
	}
	if len(cursor.Payload) < 8 || cursor.Payload[1] != 'P' {
		t.Errorf("payload is not a PNG: % x", cursor.Payload[:min(8, len(cursor.Payload))])
	}
}

func TestSessionForwardsSystemAndHiddenCursors(t *testing.T) {
	// A server that names one of its own cursors sends no pixels. A client that
	// hears nothing keeps the last shape it was given, which is how a pointer
	// ends up looking like an hourglass forever.
	_, remote, tr := newTestSession(t, testConfig())
	drainStartup(t, tr)

	remote.pushCursor(rdp.Cursor{System: true, SystemType: 1})
	waitFor(t, "the system cursor", func() bool {
		_, ok := tr.firstOfType("cursor.system").(wsframe.CursorSystem)
		return ok
	})
	sys := tr.firstOfType("cursor.system").(wsframe.CursorSystem)
	if sys.Hidden {
		t.Error("a named cursor was reported as hidden")
	}
	if sys.SystemType != 1 {
		t.Errorf("system type = %d, want 1", sys.SystemType)
	}

	tr.mu.Lock()
	tr.controls = nil
	tr.mu.Unlock()

	// SYSPTR_NULL is the only value the specification names for "no pointer".
	remote.pushCursor(rdp.Cursor{System: true, SystemType: 0})
	waitFor(t, "the hidden cursor", func() bool {
		s, ok := tr.firstOfType("cursor.system").(wsframe.CursorSystem)
		return ok && s.Hidden
	})
}

// ---------------------------------------------------------------- events

func TestSessionForwardsRemoteEvents(t *testing.T) {
	_, remote, tr := newTestSession(t, testConfig())

	remote.events <- rdp.ClipboardText{Text: "from remote", At: time.UnixMilli(1700000000000)}
	remote.events <- rdp.CursorPos{X: 11, Y: 22}
	remote.events <- rdp.ErrorEvent{Code: rdp.CodeNLAFailed, Err: errors.New("credssp failed")}

	waitFor(t, "clipboard text", func() bool { return tr.firstOfType("clipboard.text") != nil })
	waitFor(t, "cursor position", func() bool { return tr.firstOfType("cursor.pos") != nil })
	waitFor(t, "the remote error", func() bool { return tr.hasErrorWithCode(rdp.CodeNLAFailed) })

	clip := tr.firstOfType("clipboard.text").(wsframe.ClipboardText)
	if clip.Text != "from remote" || clip.TS != 1700000000000 {
		t.Errorf("clipboard = %+v", clip)
	}
	cursor := tr.firstOfType("cursor.pos").(wsframe.CursorPos)
	if cursor.X != 11 || cursor.Y != 22 {
		t.Errorf("cursor = %+v", cursor)
	}
}

func TestSessionClosedEndsEverything(t *testing.T) {
	remote := newFakeRemote()
	forgotten := make(chan string, 1)
	s := newSession("gone", testConfig(), remote, options{
		onDone: func(sess *Session) { forgotten <- sess.ID() },
	})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	remote.events <- rdp.Closed{Reason: rdp.ReasonRemoteEnded}

	waitFor(t, "the session to close", func() bool { return tr.isClosed() })

	closed, ok := tr.firstOfType("session.closed").(wsframe.SessionClosed)
	if !ok {
		t.Fatalf("no SessionClosed was sent: %v", tr.controlTypes())
	}
	if closed.Reason != wsframe.CloseRemoteEnded {
		t.Errorf("reason = %q, want %q", closed.Reason, wsframe.CloseRemoteEnded)
	}
	if !remote.isClosed() {
		t.Error("the remote connection was not released")
	}
	select {
	case id := <-forgotten:
		if id != "gone" {
			t.Errorf("forgotten id = %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Error("the registry was never told the session ended")
	}
}

func TestSessionWithNoGraceEndsWhenTheClientDisconnects(t *testing.T) {
	remote := newFakeRemote()
	s := newSession("id", testConfig(), remote, options{reconnectGrace: -1})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// A negative grace turns reconnection off, so a browser going away ends the
	// session and frees the remote desktop, exactly as it did before the grace
	// period existed.
	tr.Close()

	waitFor(t, "the session to notice", func() bool { return remote.isClosed() })

	// The reason is the assertion that matters. time.NewTimer with a negative
	// duration fires at once, so a session with no grace ends either way; only
	// the reason distinguishes "the operator closed it" from "the browser went
	// away and nobody came back", and those are different things to be told.
	closed, ok := tr.firstOfType("session.closed").(wsframe.SessionClosed)
	if !ok {
		t.Fatalf("no closing notice was sent: %v", tr.controlTypes())
	}
	if closed.Reason != wsframe.CloseUserRequested {
		t.Errorf("reason = %q, want %q", closed.Reason, wsframe.CloseUserRequested)
	}
}

// ---------------------------------------------------------------- reconnection

// newReconnectSession builds a session whose grace a test can wait out and whose
// frame interval is short enough to see a repaint promptly.
func newReconnectSession(t *testing.T, grace time.Duration, src FrameSource) (*Session, *fakeRemote) {
	t.Helper()
	remote := newFakeRemote()
	remote.src = src
	s := newSession("reconnect", testConfig(), remote, options{
		idleTimeout:    time.Minute,
		statsEvery:     time.Hour,
		idleEvery:      time.Hour,
		frameEvery:     2 * time.Millisecond,
		reconnectGrace: grace,
		now:            time.Now,
	})
	t.Cleanup(func() { s.Close(); s.finish() })
	return s, remote
}

func TestSessionSurvivesADisconnectAndReattaches(t *testing.T) {
	s, remote := newReconnectSession(t, time.Minute, nil)
	tr1 := newFakeTransport()
	if err := s.Attach(tr1); err != nil {
		t.Fatalf("first Attach: %v", err)
	}

	// The browser drops. The session stays up, holding the remote desktop.
	tr1.Close()
	waitFor(t, "the session to detach", func() bool { return !s.Info().Attached })
	if remote.isClosed() {
		t.Fatal("the remote connection was released when the browser went away")
	}

	// A new socket attaches to the same session and input works again.
	tr2 := newFakeTransport()
	if err := s.Attach(tr2); err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	if !s.Info().Attached {
		t.Error("the session does not report a browser after re-attach")
	}
	waitFor(t, "the header on the new socket", func() bool { return tr2.firstOfType("session.ready") != nil })

	tr2.send(t, wsframe.InputKey{Code: "KeyB", Down: true})
	waitFor(t, "input after re-attach", func() bool {
		return strings.Contains(strings.Join(remote.Calls(), " "), "key KeyB down")
	})
	if remote.isClosed() {
		t.Error("the remote connection was released")
	}
}

func TestSessionKeepsFramesAcrossAReconnect(t *testing.T) {
	src := newFakeSource(200, 150)
	s, _ := newReconnectSession(t, time.Minute, src)
	tr1 := newFakeTransport()
	if err := s.Attach(tr1); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// The first client is given the opening full frame.
	src.send(image.Rect(0, 0, 1, 1))
	waitFor(t, "the opening full frame", func() bool {
		frames := tr1.allFrames(t)
		return len(frames) >= 1 && frames[0].IsFull()
	})

	// The browser drops and a region changes while nobody is watching.
	tr1.Close()
	waitFor(t, "the session to detach", func() bool { return !s.Info().Attached })
	src.send(image.Rect(10, 10, 40, 40))

	// The new client holds no canvas, so the first frame it is given has to be
	// the whole desktop — covering the region that changed while it was away.
	tr2 := newFakeTransport()
	if err := s.Attach(tr2); err != nil {
		t.Fatalf("re-Attach: %v", err)
	}

	waitFor(t, "a frame on the new socket", func() bool { return len(tr2.allFrames(t)) >= 1 })
	first := tr2.allFrames(t)[0]
	if !first.IsFull() {
		t.Errorf("the first frame after re-attach is not a full repaint: %+v", first)
	}
	if first.Rect != image.Rect(0, 0, 200, 150) {
		t.Errorf("the first frame covers %v, want the whole desktop", first.Rect)
	}
}

func TestEveryReconnectStartsWithAFullRepaint(t *testing.T) {
	// The race the repaint request closes: a reconnect that lands between the
	// frame loop's check for a browser and its check for a pending repaint must
	// not let an incremental frame become the first one the new client sees.
	src := newFakeSource(160, 120)
	s, _ := newReconnectSession(t, time.Minute, src)

	current := newFakeTransport()
	if err := s.Attach(current); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	src.send(image.Rect(0, 0, 1, 1))
	waitFor(t, "the opening frame", func() bool { return len(current.allFrames(t)) >= 1 })

	for cycle := 0; cycle < 4; cycle++ {
		current.Close()
		waitFor(t, "the session to detach", func() bool { return !s.Info().Attached })

		current = newFakeTransport()
		if err := s.Attach(current); err != nil {
			t.Fatalf("cycle %d: re-Attach: %v", cycle, err)
		}
		// A region after the reconnect is enough to produce a frame; it has to
		// cover the whole desktop.
		src.send(image.Rect(5, 5, 25, 25))

		waitFor(t, "a frame after the reconnect", func() bool { return len(current.allFrames(t)) >= 1 })
		if first := current.allFrames(t)[0]; !first.IsFull() {
			t.Fatalf("cycle %d: the first frame after a reconnect is incremental: %+v", cycle, first)
		}
	}
}

func TestDetachedSessionEndsAfterTheGrace(t *testing.T) {
	s, remote := newReconnectSession(t, 40*time.Millisecond, nil)
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	tr.Close()
	waitFor(t, "the grace to expire", func() bool { return remote.isClosed() })

	// The closing notice names the reason the client can explain: the browser is
	// gone and nobody came back for the desktop.
	closed, ok := tr.firstOfType("session.closed").(wsframe.SessionClosed)
	if !ok {
		t.Fatalf("no SessionClosed was sent: %v", tr.controlTypes())
	}
	if closed.Reason != wsframe.CloseClientGone {
		t.Errorf("reason = %q, want %q", closed.Reason, wsframe.CloseClientGone)
	}
}

func TestReconnectGraceDefaultsToTheConstant(t *testing.T) {
	if DefaultReconnectGrace != 30*time.Second {
		t.Fatalf("DefaultReconnectGrace = %v, want 30s", DefaultReconnectGrace)
	}

	// A caller that does not know about reconnection gets the default; only an
	// explicit negative value turns the grace off. The two are easy to confuse,
	// which is why the zero case is pinned here.
	s := newSession("id", testConfig(), newFakeRemote(), options{})
	t.Cleanup(func() { s.Close(); s.finish() })
	if s.opts.reconnectGrace != DefaultReconnectGrace {
		t.Errorf("reconnectGrace = %v, want %v", s.opts.reconnectGrace, DefaultReconnectGrace)
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	remote := newFakeRemote()
	s := newSession("id", testConfig(), remote, options{})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, "the session to start", func() bool { return tr.firstOfType("session.ready") != nil })

	// Close races the event loop and every other goroutine that might notice
	// the end. Nothing may panic and the transport must be released once.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			s.Close()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked")
	}
	waitFor(t, "the transport to close", func() bool { return tr.isClosed() })
	waitFor(t, "the remote to be released", func() bool { return remote.isClosed() })

	closes := 0
	for _, c := range tr.controlTypes() {
		if c == "session.closed" {
			closes++
		}
	}
	if closes > 1 {
		t.Errorf("sent SessionClosed %d times, want at most once", closes)
	}
}

func TestSessionSendsStatsWithQuality(t *testing.T) {
	s, _, tr := newTestSession(t, testConfig())
	waitFor(t, "the first stats message", func() bool { return tr.firstOfType("session.stats") != nil })

	stats := tr.firstOfType("session.stats").(wsframe.SessionStats)
	if stats.Quality != s.quality.Value() {
		t.Errorf("stats quality = %d, want %d", stats.Quality, s.quality.Value())
	}
	if stats.FPS != 0 {
		// Nothing produces frames yet, so a non-zero rate would be invented.
		t.Errorf("fps = %v with no frame source, want 0", stats.FPS)
	}
}

func TestSessionStopsSendingWhenTheTransportFails(t *testing.T) {
	remote := newFakeRemote()
	s := newSession("id", testConfig(), remote, options{})
	tr := newFakeTransport()
	tr.sendErr = errors.New("broken pipe")
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	waitFor(t, "the session to give up", func() bool { return remote.isClosed() })
}

// ---------------------------------------------------------------- idle

func TestIdleTimeoutEndsTheSession(t *testing.T) {
	remote := newFakeRemote()

	clock := newTestClock(time.Now())

	s := newSession("idle", testConfig(), remote, options{
		idleTimeout: time.Minute,
		idleEvery:   10 * time.Millisecond,
		now:         clock.Now,
	})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Well inside the timeout: still alive.
	clock.Advance(30 * time.Second)
	time.Sleep(40 * time.Millisecond)
	if tr.isClosed() {
		t.Fatal("the session ended before the idle timeout")
	}

	clock.Advance(2 * time.Minute)
	waitFor(t, "the idle timeout", func() bool { return tr.isClosed() })

	closed, _ := tr.firstOfType("session.closed").(wsframe.SessionClosed)
	if closed.Reason != wsframe.CloseIdleTimeout {
		t.Errorf("reason = %q, want %q", closed.Reason, wsframe.CloseIdleTimeout)
	}
}

func TestHeartbeatsDoNotDefeatTheIdleTimeout(t *testing.T) {
	remote := newFakeRemote()

	clock := newTestClock(time.Now())
	s := newSession("idle", testConfig(), remote, options{
		idleTimeout: time.Minute,
		idleEvery:   10 * time.Millisecond,
		now:         clock.Now,
	})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// A tab left open sends pings forever. If those counted as activity, a
	// session nobody is watching would hold a remote desktop open all night.
	for i := 0; i < 5; i++ {
		tr.send(t, wsframe.Ping{T: int64(i)})
		waitFor(t, "the pong", func() bool { return tr.countOfType("pong") > i })
		clock.Advance(30 * time.Second)
	}

	waitFor(t, "the idle timeout despite pings", func() bool { return tr.isClosed() })
}

func TestInputResetsTheIdleTimer(t *testing.T) {
	remote := newFakeRemote()

	clock := newTestClock(time.Now())
	s := newSession("idle", testConfig(), remote, options{
		idleTimeout: time.Minute,
		idleEvery:   10 * time.Millisecond,
		now:         clock.Now,
	})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	for i := 0; i < 6; i++ {
		clock.Advance(30 * time.Second)
		tr.send(t, wsframe.InputMouse{X: i, Y: i, Action: wsframe.PointerMove})
		waitFor(t, "the move to be recorded", func() bool { return len(remote.Calls()) > i })
		time.Sleep(20 * time.Millisecond)
		if tr.isClosed() {
			t.Fatalf("the session expired despite input at %v", clock.Now())
		}
	}
}

func TestIdleTimeoutCanBeDisabled(t *testing.T) {
	remote := newFakeRemote()
	clock := newTestClock(time.Now())

	s := newSession("idle", testConfig(), remote, options{
		idleTimeout: -1,
		idleEvery:   10 * time.Millisecond,
		now:         clock.Now,
	})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	clock.Advance(24 * time.Hour)
	time.Sleep(40 * time.Millisecond)
	if tr.isClosed() {
		t.Error("a negative idle timeout must disable the idle check")
	}
}

// ---------------------------------------------------------------- frames

// fakeSource is a FrameSource a test drives by hand.
type fakeSource struct {
	mu      sync.Mutex
	bounds  image.Rectangle
	updates chan Update
	closed  bool
}

func newFakeSource(w, h int) *fakeSource {
	return &fakeSource{
		bounds:  image.Rect(0, 0, w, h),
		updates: make(chan Update, 8),
	}
}

func (f *fakeSource) Bounds() image.Rectangle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bounds
}

func (f *fakeSource) Updates() <-chan Update { return f.updates }

func (f *fakeSource) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.updates)
	}
	return nil
}

// send publishes a batch the way a source does: an image of the desktop, and
// the regions that changed in it.
func (f *fakeSource) send(regions ...image.Rectangle) {
	f.sendColour(color.RGBA{R: 200, G: 40, B: 40, A: 255}, regions...)
}

// sendColour is send with a colour, so a test can tell which frame it is looking
// at after two of them were merged.
func (f *fakeSource) sendColour(c color.RGBA, regions ...image.Rectangle) {
	f.mu.Lock()
	bounds := f.bounds
	img := image.NewRGBA(bounds)
	f.mu.Unlock()

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	f.updates <- Update{Image: img, Regions: regions}
}

// resizeTo changes the desktop size, as a display control resize would.
func (f *fakeSource) resizeTo(w, h int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bounds = image.Rect(0, 0, w, h)
}

// newSessionWithSource attaches a session whose remote supplies pixels.
func newSessionWithSource(t *testing.T, cfg rdp.Config, src FrameSource) (*Session, *fakeRemote, *fakeTransport) {
	t.Helper()
	remote := newFakeRemote()
	remote.src = src
	s := newSession("test-id", cfg, remote, options{
		idleTimeout: time.Minute,
		statsEvery:  10 * time.Millisecond,
		idleEvery:   10 * time.Millisecond,
		frameEvery:  2 * time.Millisecond,
		now:         time.Now,
	})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { s.Close(); s.finish() })
	return s, remote, tr
}

// allFrames returns every frame sent so far.
// batchCount is how many times SendFrames was called, which is one per encode.
func (f *fakeTransport) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames)
}

func (f *fakeTransport) allFrames(t *testing.T) []wsframe.Frame {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []wsframe.Frame
	for _, batch := range f.frames {
		out = append(out, batch...)
	}
	return out
}

// prime consumes the mandatory opening full frame so a test can go on to assert
// what a region update does.
func prime(t *testing.T, src *fakeSource, tr *fakeTransport) int {
	t.Helper()
	src.send(image.Rect(0, 0, 1, 1))
	waitFor(t, "the opening full frame", func() bool {
		frames := tr.allFrames(t)
		return len(frames) >= 1 && frames[0].IsFull()
	})
	return len(tr.allFrames(t))
}

func TestSessionEncodesFramesFromItsSource(t *testing.T) {
	src := newFakeSource(320, 200)
	_, _, tr := newSessionWithSource(t, testConfig(), src)

	seen := prime(t, src, tr)

	src.send(image.Rect(10, 20, 60, 70))
	waitFor(t, "the region update", func() bool { return len(tr.allFrames(t)) > seen })

	frames := tr.allFrames(t)[seen:]
	first := frames[0]
	if first.IsFull() {
		t.Fatal("a region update was sent as a full frame")
	}
	if first.Type != wsframe.TypeImage {
		t.Errorf("type = %#x, want an image frame", first.Type)
	}
	// The encoder is a setting rather than a constant, so this says the format
	// byte names a real image format and that the payload really is one, instead
	// of naming whichever encoder happens to be the default this month. A format
	// byte with nothing behind it is the fault worth catching.
	switch first.Format {
	case wsframe.FormatPNG, wsframe.FormatJPEG, wsframe.FormatWebP:
	default:
		t.Errorf("format = %#x, which is not an image format", first.Format)
	}
	if first.Rect != image.Rect(10, 20, 60, 70) {
		t.Errorf("rect = %v, want the dirty region", first.Rect)
	}
	if first.Flags&wsframe.FlagKeyframe != 0 {
		t.Error("only the first frame of a stream is a keyframe")
	}

	// And the bytes have to decode, which is what the client will ask of them.
	if _, _, err := image.Decode(bytes.NewReader(first.Payload)); err != nil {
		t.Errorf("the payload does not decode as an image: %v", err)
	}
}

func TestSessionMergesDirtyRegionsBeforeEncoding(t *testing.T) {
	// The whole point of accumulating: a scroll and a caret in one tick should
	// not become a frame per region when they touch.
	src := newFakeSource(400, 300)
	_, _, tr := newSessionWithSource(t, testConfig(), src)
	seen := prime(t, src, tr)

	src.send(
		image.Rect(0, 0, 100, 100),
		image.Rect(100, 0, 200, 100), // shares an edge with the first
		image.Rect(300, 200, 400, 300),
	)
	waitFor(t, "the encoded batch", func() bool { return len(tr.allFrames(t)) > seen })

	frames := tr.allFrames(t)[seen:]
	if len(frames) != 2 {
		t.Fatalf("got %d frames %v, want 2: the touching pair should merge", len(frames), frames)
	}
	if frames[0].Rect != image.Rect(0, 0, 200, 100) {
		t.Errorf("first rect = %v, want the merged pair", frames[0].Rect)
	}
	if frames[1].Rect != image.Rect(300, 200, 400, 300) {
		t.Errorf("second rect = %v", frames[1].Rect)
	}
}

func TestSessionPromotesAFullRepaint(t *testing.T) {
	src := newFakeSource(200, 200)
	_, _, tr := newSessionWithSource(t, testConfig(), src)
	seen := prime(t, src, tr)

	// 150x150 of a 200x200 desktop is 56%, so the second, overlapping region is
	// what crosses the 60% threshold and collapses the batch into one repaint.
	src.send(
		image.Rect(0, 0, 150, 150),
		image.Rect(50, 50, 200, 200),
	)
	waitFor(t, "the promoted frame", func() bool { return len(tr.allFrames(t)) > seen })

	frames := tr.allFrames(t)[seen:]
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 full frame", len(frames))
	}
	if !frames[0].IsFull() {
		t.Error("the frame is not marked full")
	}
	if frames[0].Rect != image.Rect(0, 0, 200, 200) {
		t.Errorf("rect = %v, want the whole desktop", frames[0].Rect)
	}
}

func TestSessionClosesTheFrameSource(t *testing.T) {
	src := newFakeSource(64, 64)
	s, _, _ := newSessionWithSource(t, testConfig(), src)

	s.Close()
	waitFor(t, "the source to be closed", src.isClosed)
}

func TestSessionReportsNoPictureWhenTheSourceIsMissing(t *testing.T) {
	// Without a source the client has to be told, or it waits forever for a
	// frame that is never coming.
	remote := newFakeRemote() // src stays nil, so the grdp surface is used
	s := newSession("no-source", testConfig(), remote, options{idleTimeout: time.Minute})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { s.Close(); s.finish() })

	waitFor(t, "the capability report", func() bool {
		return len(tr.messagesMentioning("requirement A")) > 0
	})
	if frames := tr.allFrames(t); len(frames) != 0 {
		t.Errorf("sent %d frames with no source", len(frames))
	}
}

// ---------------------------------------------------------------- demo

func TestDemoRemoteDrawsTheInputItReceives(t *testing.T) {
	cfg := rdp.Config{Name: "demo", Width: 640, Height: 480}
	remote := newDemoRemote(cfg, nil)
	defer remote.Close()

	if err := remote.Key("KeyA", true); err != nil {
		t.Fatalf("Key: %v", err)
	}
	remote.MouseButton(0, true, 120, 240)
	remote.MouseWheel(-2, 120, 240)

	src, err := remote.FrameSource()
	if err != nil {
		t.Fatalf("the demo remote has no frame source: %v", err)
	}
	demo, ok := src.(*demoSource)
	if !ok {
		t.Fatalf("frame source is %T", src)
	}

	// The ticker redraws on its own cadence; step it directly so the assertion
	// does not depend on timing.
	demo.step()

	lines := demo.echoLines()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"KeyA", "down", "mouse", "wheel"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the echo does not mention %q:\n%s", want, joined)
		}
	}
}

func TestDemoRemoteRefusesOnlyWhatItCannotDo(t *testing.T) {
	// The demo must not look more capable than the gateway is, or it stops
	// being a way to check the UI and starts being a way to be misled.
	remote := newDemoRemote(rdp.Config{}, nil)
	defer remote.Close()

	// Resizing is the one thing the synthetic desktop cannot do, and it says so
	// instead of accepting the request and drawing nothing.
	if err := remote.RequestResize(1920, 1080); !errors.Is(err, rdp.ErrUnsupported) {
		t.Errorf("RequestResize = %v, want ErrUnsupported", err)
	}
	// Composer input works, so the demo must not refuse it.
	if err := remote.UnicodeKey('中'); err != nil {
		t.Errorf("UnicodeKey = %v, want nil: the feature landed", err)
	}
	// The demo hands over pixels through a frame source, not the framebuffer.
	if _, err := remote.Framebuffer(); !errors.Is(err, rdp.ErrUnsupported) {
		t.Errorf("Framebuffer = %v, want ErrUnsupported", err)
	}
}

func TestDemoSourceProducesUpdatesOnItsOwn(t *testing.T) {
	// The animation has to drive the encoder without anyone poking it, or the
	// demo would only move when the operator clicks.
	src := newDemoSource(nil)
	defer src.Close()

	select {
	case update := <-src.Updates():
		if len(update.Regions) == 0 {
			t.Fatal("an update arrived with no regions")
		}
		if update.Image == nil {
			t.Fatal("an update arrived with no image")
		}
		if got := update.Image.Bounds(); got != src.Bounds() {
			t.Errorf("update image is %v, want the desktop %v", got, src.Bounds())
		}
		// The buffer must survive until it is released: reading it here is the
		// point of the ownership handoff.
		if update.Image.(*image.RGBA).Pix[0] == 0 && update.Image.(*image.RGBA).Pix[1] == 0 &&
			update.Image.(*image.RGBA).Pix[2] == 0 {
			t.Error("the published frame is black; the canvas was not copied")
		}
		countBefore := len(src.pool.free)
		update.Release()
		if len(src.pool.free) <= countBefore {
			t.Error("Release did not return the buffer to the pool")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the demo source produced no updates")
	}
}

func TestDemoSourceEchoIsBounded(t *testing.T) {
	src := newDemoSource(nil)
	defer src.Close()

	for i := 0; i < demoEchoRows*3; i++ {
		src.ObserveKey(fmt.Sprintf("Key%d", i), true)
	}
	if got := len(src.echoLines()); got != demoEchoRows {
		t.Errorf("the echo holds %d lines, want at most %d", got, demoEchoRows)
	}
	// Newest first, so the operator sees the last thing they pressed.
	src.ObserveKey("KeyZ", true)
	if first := src.echoLines()[0]; !strings.Contains(first, "KeyZ") {
		t.Errorf("the newest event is not first: %q", first)
	}
}

func TestDemoSourceClosesOnce(t *testing.T) {
	src := newDemoSource(nil)
	src.Close()
	src.Close()
	src.Close()
}

func TestRegistryDemoModeNeedsNoNetwork(t *testing.T) {
	// The whole point of demo mode: no dial, no credentials, and a picture.
	r, dialer := newTestRegistry(t, Options{Max: 2, Demo: true})

	s, err := r.Create(context.Background(), rdp.Config{Name: "demo", Width: 640, Height: 480})
	if err != nil {
		t.Fatalf("Create in demo mode: %v", err)
	}
	if dialer.DialCount() != 0 {
		t.Errorf("demo mode dialled %d times; it must not touch the network", dialer.DialCount())
	}

	info := s.Info()
	if info.Width != demoWidth || info.Height != demoHeight {
		t.Errorf("session is %dx%d, want the synthetic desktop %dx%d",
			info.Width, info.Height, demoWidth, demoHeight)
	}

	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, "a frame from the demo desktop", func() bool { return len(tr.allFrames(t)) >= 1 })
	waitFor(t, "the demo ready message", func() bool { return tr.firstOfType("session.ready") != nil })

	ready := tr.firstOfType("session.ready").(wsframe.SessionReady)
	if ready.Width != demoWidth || ready.Height != demoHeight {
		t.Errorf("ready reports %dx%d, want %dx%d", ready.Width, ready.Height, demoWidth, demoHeight)
	}
	if ready.Protocol != "demo" {
		t.Errorf("protocol = %q, want demo so the UI can say so", ready.Protocol)
	}

	s.Close()
}

func TestDemoDesktopIsActuallyDrawn(t *testing.T) {
	// The demo exists to be looked at, so the shapes have to be where the code
	// says they are. Sampling the pixels is the only assertion that catches a
	// draw call that silently lands outside the image.
	src := newDemoSource(nil)
	defer src.Close()

	src.mu.Lock()
	img := src.canvas
	src.mu.Unlock()

	teal := color.RGBA{0, 128, 128, 255}
	face := color.RGBA{0xD4, 0xD0, 0xC8, 255}

	cases := []struct {
		name string
		x, y int
		want color.RGBA
	}{
		{"desktop outside the window", 5, 5, teal},
		{"window body, above the echo box", 500, 100, face},
		{"window body, left of the echo box", 30, 300, face},
		{"echo box is white", 500, 300, color.RGBA{255, 255, 255, 255}},
		{"below the window is desktop again", 500, 610, teal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := img.RGBAAt(tc.x, tc.y)
			if got != tc.want {
				t.Errorf("pixel (%d,%d) = %v, want %v", tc.x, tc.y, got, tc.want)
			}
		})
	}
}

func TestTheFirstFrameIsAFullRepaint(t *testing.T) {
	// The client's canvas starts empty, so a stream of dirty rectangles draws
	// nothing recognisable: the desktop has to be established once before any
	// region update means anything. Getting this wrong is invisible in a
	// screenshot of a page whose canvas happens to be pre-filled, and obvious
	// on a real one.
	src := newFakeSource(200, 150)
	_, _, tr := newSessionWithSource(t, testConfig(), src)

	// A single small change is all the source reports.
	src.send(image.Rect(10, 10, 30, 30))

	waitFor(t, "the first frame", func() bool { return len(tr.allFrames(t)) >= 1 })

	first := tr.allFrames(t)[0]
	if !first.IsFull() {
		t.Errorf("the first frame is not a full repaint: %+v", first)
	}
	if first.Rect != image.Rect(0, 0, 200, 150) {
		t.Errorf("the first frame covers %v, want the whole desktop", first.Rect)
	}
	if first.Flags&wsframe.FlagKeyframe == 0 {
		t.Error("the first frame is not marked as a keyframe")
	}

	// The region itself must not be lost: it is part of what was sent.
	src.send(image.Rect(40, 40, 60, 60))
	waitFor(t, "a later region update", func() bool { return len(tr.allFrames(t)) >= 2 })

	second := tr.allFrames(t)[1]
	if second.IsFull() {
		t.Error("a later region update was sent as a full frame")
	}
	if second.Rect != image.Rect(40, 40, 60, 60) {
		t.Errorf("second frame rect = %v, want the dirty region", second.Rect)
	}
}

// lastOfType returns the most recent control of a type, or nil.
func (f *fakeTransport) lastOfType(kind string) wsframe.Control {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.controls) - 1; i >= 0; i-- {
		if f.controls[i].ControlType() == kind {
			return f.controls[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------- file transfer

// fakeFiles is an rdp.Files a test drives. It records what the session offered
// and answers reads from a table.
type fakeFiles struct {
	mu       sync.Mutex
	provider rdp.FileProvider
	manifest []rdp.RemoteFile
	onRemote func([]rdp.RemoteFile)
	content  map[uint32][]byte
	readErr  error
	begun    bool
}

func (f *fakeFiles) Offer(p rdp.FileProvider) {
	f.mu.Lock()
	f.provider = p
	f.mu.Unlock()
}

func (f *fakeFiles) OnRemoteFiles(cb func([]rdp.RemoteFile)) error {
	f.mu.Lock()
	f.onRemote = cb
	f.begun = true
	f.mu.Unlock()
	return nil
}

func (f *fakeFiles) ReadRemoteFile(index uint32, off int64, n int) ([]byte, error) {
	f.mu.Lock()
	err := f.readErr
	data := f.content[index]
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if off < 0 || off >= int64(len(data)) {
		return nil, nil
	}
	end := off + int64(n)
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return data[off:end], nil
}

func (f *fakeFiles) ClearRemoteFiles() {}

func (f *fakeFiles) offered() rdp.FileProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.provider
}

// announce makes the remote offer its files, the way a paste on the remote
// would.
func (f *fakeFiles) announce(files []rdp.RemoteFile) {
	f.mu.Lock()
	f.manifest = files
	cb := f.onRemote
	f.mu.Unlock()
	if cb != nil {
		cb(files)
	}
}

func newSessionWithFiles(t *testing.T, files *fakeFiles) (*Session, *fakeTransport) {
	t.Helper()
	remote := newFakeRemote()
	remote.files = files
	s := newSession("files", testConfig(), remote, options{
		idleTimeout: time.Minute,
		statsEvery:  time.Hour,
		idleEvery:   time.Hour,
		frameEvery:  2 * time.Millisecond,
		now:         time.Now,
	})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { s.Close(); s.finish() })
	waitFor(t, "the file transfer to subscribe", func() bool {
		files.mu.Lock()
		defer files.mu.Unlock()
		return files.begun
	})
	return s, tr
}

func TestSessionForwardsTheRemoteManifest(t *testing.T) {
	files := &fakeFiles{}
	_, tr := newSessionWithFiles(t, files)

	files.announce([]rdp.RemoteFile{
		{Index: 0, Name: "report.pdf", Size: 4096},
		{Index: 1, Name: "logs", Size: 0, IsDir: true},
	})
	waitFor(t, "the manifest to reach the client", func() bool {
		return tr.firstOfType("files.remote") != nil
	})

	msg, ok := tr.firstOfType("files.remote").(wsframe.FilesRemote)
	if !ok {
		t.Fatalf("got %T", tr.firstOfType("files.remote"))
	}
	if len(msg.Files) != 2 {
		t.Fatalf("manifest has %d entries, want 2", len(msg.Files))
	}
	if msg.Files[0].Name != "report.pdf" || msg.Files[0].Size != 4096 {
		t.Errorf("first entry = %+v", msg.Files[0])
	}
	if !msg.Files[1].IsDir {
		t.Error("the directory flag was lost on the way to the client")
	}
}

func TestSessionOffersFilesAndPullsTheirRanges(t *testing.T) {
	// The whole upload path in one test: the client offers a file, the gateway
	// installs a provider, and reading that provider — which is what the
	// remote's paste does — comes back to the client as a range request.
	files := &fakeFiles{}
	_, tr := newSessionWithFiles(t, files)
	drainStartup(t, tr)

	content := []byte("the bytes of an offered file, read back a range at a time")
	tr.send(t, wsframe.FilesOffer{Files: []wsframe.OfferedFile{
		{Name: "notes.txt", Size: int64(len(content))},
	}})

	waitFor(t, "the provider to be installed", func() bool { return files.offered() != nil })
	provider := files.offered()
	if names := provider.Names(); len(names) != 1 || names[0] != "notes.txt" {
		t.Fatalf("Names() = %v", names)
	}
	if size, _, isDir := provider.Info("notes.txt"); size != int64(len(content)) || isDir {
		t.Errorf("Info = %d, dir=%v", size, isDir)
	}

	f, err := provider.Open("notes.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	seeker, ok := f.(io.Seeker)
	if !ok {
		t.Fatal("the reader does not seek, so the protocol layer would read and discard to reach an offset")
	}
	const off = 10
	if _, err := seeker.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	// The read blocks until the client answers, so answer from another
	// goroutine once the request is visible.
	read := make(chan []byte, 1)
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 20)
		n, err := f.Read(buf)
		if err != nil {
			readErr <- err
			return
		}
		read <- buf[:n]
	}()

	waitFor(t, "the range request", func() bool { return tr.lastOfType("files.need") != nil })
	need, ok := tr.lastOfType("files.need").(wsframe.FilesNeed)
	if !ok {
		t.Fatalf("got %T", tr.lastOfType("files.need"))
	}
	if need.Name != "notes.txt" || need.Offset != off {
		t.Errorf("request = %+v, want %q at %d", need, "notes.txt", off)
	}
	if need.Length < 20 {
		t.Errorf("request length = %d, want at least the 20 bytes being read", need.Length)
	}

	// Answer with the file's bytes from that offset.
	want := content[off : off+20]
	tr.sendChunk(need.Token, uint32(off), want)

	select {
	case got := <-read:
		if string(got) != string(want) {
			t.Errorf("read %q, want %q", got, want)
		}
	case err := <-readErr:
		t.Fatalf("Read: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Read never returned: the range was not delivered")
	}

	// An offer of nothing withdraws the provider: a format the client can no
	// longer produce data for must stop being advertised.
	tr.send(t, wsframe.FilesClear{})
	waitFor(t, "the offer to be withdrawn", func() bool { return files.offered() == nil })
}

func TestSessionStreamsADownloadInChunks(t *testing.T) {
	// A file larger than one chunk, so the chunking is actually exercised.
	payload := make([]byte, 2*downloadChunk+1234)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	files := &fakeFiles{content: map[uint32][]byte{3: payload}}
	_, tr := newSessionWithFiles(t, files)
	drainStartup(t, tr)

	files.announce([]rdp.RemoteFile{{Index: 3, Name: "big.bin", Size: int64(len(payload))}})
	waitFor(t, "the manifest", func() bool { return tr.firstOfType("files.remote") != nil })

	tr.send(t, wsframe.FilesGet{Index: 3})
	waitFor(t, "the download to start", func() bool { return tr.firstOfType("files.begin") != nil })

	begin, _ := tr.firstOfType("files.begin").(wsframe.FilesBegin)
	if begin.Name != "big.bin" || begin.Size != int64(len(payload)) {
		t.Fatalf("begin = %+v", begin)
	}

	waitFor(t, "the download to finish", func() bool { return tr.firstOfType("files.end") != nil })
	end, _ := tr.firstOfType("files.end").(wsframe.FilesEnd)
	if end.Error != "" {
		t.Fatalf("the download failed: %s", end.Error)
	}
	if end.TransferID != begin.TransferID {
		t.Errorf("the end names transfer %d, the begin named %d", end.TransferID, begin.TransferID)
	}

	// Reassemble from the chunks and check the bytes arrived in one piece, in
	// order, at the offsets the file expects.
	got := make([]byte, len(payload))
	seen := 0
	for _, f := range tr.allFrames(t) {
		if f.Type != wsframe.TypeFile {
			continue
		}
		token, offset, data, err := wsframe.ParseFileChunk(f)
		if err != nil {
			t.Fatalf("ParseFileChunk: %v", err)
		}
		if token != begin.TransferID {
			t.Errorf("chunk names transfer %d, want %d", token, begin.TransferID)
		}
		if int(offset)+len(data) > len(got) {
			t.Fatalf("chunk at %d with %d bytes runs past the file", offset, len(data))
		}
		copy(got[offset:], data)
		seen += len(data)
	}
	if seen != len(payload) {
		t.Fatalf("chunks carried %d bytes, want %d", seen, len(payload))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte %d is %d, want %d", i, got[i], payload[i])
		}
	}
}

func TestSessionReportsADownloadFailure(t *testing.T) {
	files := &fakeFiles{
		content: map[uint32][]byte{0: []byte("x")},
		readErr: errors.New("the remote stopped answering"),
	}
	_, tr := newSessionWithFiles(t, files)
	drainStartup(t, tr)
	files.announce([]rdp.RemoteFile{{Index: 0, Name: "broken.bin", Size: 1024}})
	waitFor(t, "the manifest", func() bool { return tr.firstOfType("files.remote") != nil })

	tr.send(t, wsframe.FilesGet{Index: 0})
	waitFor(t, "the failure to be reported", func() bool {
		e, ok := tr.firstOfType("files.end").(wsframe.FilesEnd)
		return ok && e.Error != ""
	})

	end, _ := tr.firstOfType("files.end").(wsframe.FilesEnd)
	if !strings.Contains(end.Error, "stopped answering") {
		t.Errorf("end error = %q, want the reason from the remote", end.Error)
	}
}

func TestSessionRefusesToDownloadADirectory(t *testing.T) {
	files := &fakeFiles{}
	_, tr := newSessionWithFiles(t, files)
	drainStartup(t, tr)

	files.announce([]rdp.RemoteFile{{Index: 0, Name: "logs", IsDir: true}})
	waitFor(t, "the manifest", func() bool { return tr.firstOfType("files.remote") != nil })

	tr.send(t, wsframe.FilesGet{Index: 0})
	waitFor(t, "the refusal", func() bool {
		e, ok := tr.firstOfType("files.end").(wsframe.FilesEnd)
		return ok && strings.Contains(e.Error, "directory")
	})

	// Nothing was streamed: no begin, no chunks.
	if tr.firstOfType("files.begin") != nil {
		t.Error("a directory started a download")
	}
	for _, f := range tr.allFrames(t) {
		if f.Type == wsframe.TypeFile {
			t.Error("a directory produced file chunks")
		}
	}
}

func TestSessionFileTransferIsOptional(t *testing.T) {
	// A transport without file transfer must say so rather than accept an offer
	// and silently do nothing with it.
	_, remote, tr := newTestSession(t, testConfig())
	drainStartup(t, tr)
	if remote.Files() != nil {
		t.Fatal("the test fake has a file surface without one being set")
	}

	tr.send(t, wsframe.FilesOffer{Files: []wsframe.OfferedFile{{Name: "a", Size: 1}}})
	waitFor(t, "the explanation", func() bool {
		return len(tr.messagesMentioning("file transfer is unavailable")) > 0
	})
}

// decodeAverage decodes a JPEG frame and averages its colour, which is enough to
// tell which of two solid frames it is.
func decodeAverage(t *testing.T, payload []byte) (r, g, b float64) {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("the frame is not an image: %v", err)
	}
	bounds := img.Bounds()
	var sr, sg, sb float64
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			sr += float64(cr >> 8)
			sg += float64(cg >> 8)
			sb += float64(cb >> 8)
		}
	}
	n := float64(bounds.Dx() * bounds.Dy())
	return sr / n, sg / n, sb / n
}

// ---------------------------------------------------------------- pacing

// newPacedSession builds a session whose frame interval is wide enough for a
// test to push several updates inside one of them.
func newPacedSession(t *testing.T, src FrameSource, frameEvery time.Duration) (*Session, *fakeTransport) {
	t.Helper()
	remote := newFakeRemote()
	remote.src = src
	s := newSession("paced", testConfig(), remote, options{
		idleTimeout: time.Minute,
		statsEvery:  time.Hour,
		idleEvery:   time.Hour,
		frameEvery:  frameEvery,
		now:         time.Now,
	})
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { s.Close(); s.finish() })
	return s, tr
}

func TestABurstOfUpdatesBecomesFarFewerEncodes(t *testing.T) {
	// Encoding is the expensive part of a session: a full 1280x720 frame costs
	// about 32 ms. A server that draws sixty times a second must not become
	// sixty encodes, so a burst inside one interval is merged.
	const updates = 40
	src := newFakeSource(400, 300)
	_, tr := newPacedSession(t, src, 200*time.Millisecond)

	for i := 0; i < updates; i++ {
		x := (i * 7) % 300
		y := (i * 11) % 200
		src.send(image.Rect(x, y, x+30, y+20))
	}

	waitFor(t, "the first frame", func() bool { return tr.batchCount() >= 1 })
	time.Sleep(250 * time.Millisecond)

	batches := tr.batchCount()
	if batches >= updates {
		t.Errorf("%d updates produced %d encodes; the interval is not merging them",
			updates, batches)
	}
	if batches > 4 {
		t.Errorf("%d updates produced %d encodes over ~200ms at a 200ms interval",
			updates, batches)
	}
	t.Logf("%d updates became %d encodes", updates, batches)
}

func TestAMergedFrameStillCarriesEveryRegion(t *testing.T) {
	// The newer frame supersedes the older one's pixels. It does not supersede
	// its news: a client never told about a region keeps showing what used to
	// be there, and the region would stay wrong until something else redrew it.
	src := newFakeSource(400, 300)
	_, tr := newPacedSession(t, src, 300*time.Millisecond)

	first := image.Rect(10, 10, 60, 40)
	second := image.Rect(300, 250, 380, 290)
	src.send(first)
	time.Sleep(20 * time.Millisecond)
	src.send(second)

	waitFor(t, "a frame", func() bool { return tr.batchCount() >= 1 })
	time.Sleep(350 * time.Millisecond)

	frames := tr.allFrames(t)
	if len(frames) == 0 {
		t.Fatal("nothing was sent")
	}

	covered := image.Rectangle{}
	full := false
	for _, f := range frames {
		if f.IsFull() {
			full = true
			break
		}
		covered = covered.Union(f.Rect)
	}
	if !full {
		if !first.In(covered) {
			t.Errorf("the first region %v was never sent; the frame covers %v", first, covered)
		}
		if !second.In(covered) {
			t.Errorf("the second region %v was never sent; the frame covers %v", second, covered)
		}
	}
	// And it should have been one encode, not two.
	if got := tr.batchCount(); got != 1 {
		t.Errorf("two updates in one interval produced %d encodes, want 1", got)
	}
}

func TestTheNewestFrameSupersedesTheOlder(t *testing.T) {
	// Pixels, unlike news, are superseded: the held frame is dropped and the
	// newer one encoded, so what a client draws is the current desktop and not
	// a mixture of two moments.
	src := newFakeSource(200, 150)
	_, tr := newPacedSession(t, src, 300*time.Millisecond)

	red := color.RGBA{R: 220, G: 20, B: 20, A: 255}
	green := color.RGBA{R: 20, G: 220, B: 20, A: 255}
	src.sendColour(red, image.Rect(0, 0, 100, 100))
	time.Sleep(20 * time.Millisecond)
	src.sendColour(green, image.Rect(100, 50, 200, 150))

	waitFor(t, "a frame", func() bool { return tr.batchCount() >= 1 })
	time.Sleep(350 * time.Millisecond)

	frames := tr.allFrames(t)
	if len(frames) == 0 {
		t.Fatal("nothing was sent")
	}

	// The first update's region must be drawn in the *second* frame's colour,
	// because the second image is the whole desktop as of later.
	inFirst := false
	for _, f := range frames {
		if f.Rect.Overlaps(image.Rect(0, 0, 100, 100)) || f.IsFull() {
			inFirst = true
		}
	}
	if !inFirst {
		t.Fatal("the first region was never redrawn, so the stale colour would stay on screen")
	}

	// Decode the frame covering the first region and check what colour it is.
	var target *wsframe.Frame
	for i := range frames {
		if frames[i].IsFull() || frames[i].Rect.Overlaps(image.Rect(0, 0, 100, 100)) {
			target = &frames[i]
			break
		}
	}
	if target == nil {
		t.Fatal("no frame covers the first region")
	}
	r, g, b := decodeAverage(t, target.Payload)
	if !(g > r && g > b) {
		t.Errorf("the frame is not the newer colour: rgb(%.0f,%.0f,%.0f), want green", r, g, b)
	}
}

// ---------------------------------------------------------------- reconnect

// attachedReconnectSession is newReconnectSession with a browser already on it,
// which is what a test that disconnects and re-attaches needs to start from.
func attachedReconnectSession(t *testing.T, grace time.Duration, src FrameSource) (*Session, *fakeRemote, *fakeTransport) {
	t.Helper()
	s, remote := newReconnectSession(t, grace, src)
	tr := newFakeTransport()
	if err := s.Attach(tr); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, "the session to start", func() bool { return tr.firstOfType("session.ready") != nil })
	return s, remote, tr
}

// isDetached reports whether the session currently has no browser on it.
func (s *Session) isDetached() bool { return !s.Info().Attached }

func TestAReattachRepaintsAQuiescentDesktop(t *testing.T) {
	// The decisive case for a reload onto a screen nothing is drawing on. The
	// source says nothing after the first frame, so a session that keeps no
	// image has nothing to repaint from and the client waits forever on a blank
	// canvas. A moving desktop hides this, which is exactly why the demo did.
	src := newFakeSource(200, 150)
	s, _, tr := attachedReconnectSession(t, time.Minute, src)

	src.send(image.Rect(0, 0, 200, 150))
	waitFor(t, "the opening frame", func() bool { return len(tr.allFrames(t)) >= 1 })
	// Let the loop settle so nothing is in flight when the browser goes.
	time.Sleep(40 * time.Millisecond)

	tr.Close()
	waitFor(t, "the detach", s.isDetached)

	// Nothing is sent to the source from here on. Everything the new client
	// gets has to come from what the session already holds.
	fresh := newFakeTransport()
	if err := s.Attach(fresh); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	waitFor(t, "a repaint with no new update", func() bool { return len(fresh.allFrames(t)) >= 1 })

	first := fresh.allFrames(t)[0]
	if !first.IsFull() {
		t.Errorf("the first frame after a re-attach is not a full repaint: %+v", first)
	}
	if first.Rect != image.Rect(0, 0, 200, 150) {
		t.Errorf("the repaint covers %v, want the whole desktop", first.Rect)
	}
}

func TestAStaleGraceTimerDoesNotEndASessionThatReconnected(t *testing.T) {
	// The epoch guard's purpose, and the only interleaving that needs it.
	//
	// A session is attached, then detached, then re-attached, then detached
	// again. The first detach left a timer behind, and it fires while the
	// session is unattached for the second reason — so the plain "is anybody
	// attached" test is not enough, and without the epoch the first timer ends a
	// session whose second grace has barely started. The visible effect is a
	// reconnect window that shrinks every time the browser blips.
	// The arithmetic the assertions below depend on, so the window is not a
	// guess. t0 is the first detach.
	//
	//	grace              = 500ms
	//	first detach       = t0          -> its timer fires at t0+500ms
	//	second detach      = t0+300ms    -> its timer fires at t0+800ms
	//	the check          = t0+600ms    -> past the first, short of the second
	const (
		grace        = 500 * time.Millisecond
		betweenGap   = 300 * time.Millisecond
		checkAfterT2 = 300 * time.Millisecond
	)

	src := newFakeSource(64, 64)
	s, _, first := attachedReconnectSession(t, grace, src)

	first.Close()
	waitFor(t, "the first detach", s.isDetached)

	second := newFakeTransport()
	if err := s.Attach(second); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	time.Sleep(betweenGap)

	// Detached again, with its own full grace ahead of it.
	second.Close()
	waitFor(t, "the second detach", s.isDetached)

	time.Sleep(checkAfterT2)

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		t.Fatal("a grace timer from an earlier detach ended the session, cutting the second grace short")
	}

	// And the second grace does still end it, so the guard is not simply
	// disabling the expiry.
	waitFor(t, "the second grace to expire", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closed
	})
}

func TestAReattachDoesNotStartASecondFrameLoop(t *testing.T) {
	// Two frame loops would encode every update twice and send it twice. The
	// started guard is what prevents it, and nothing else in the suite would
	// notice if it went away.
	src := newFakeSource(64, 64)
	s, _, tr := attachedReconnectSession(t, time.Minute, src)
	time.Sleep(30 * time.Millisecond)

	tr.Close()
	waitFor(t, "the detach", s.isDetached)
	fresh := newFakeTransport()
	if err := s.Attach(fresh); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	before := fresh.batchCount()
	src.send(image.Rect(0, 0, 32, 32))
	waitFor(t, "the update", func() bool { return fresh.batchCount() > before })

	// Give a hypothetical second loop time to publish the same regions again.
	time.Sleep(50 * time.Millisecond)
	if got := fresh.batchCount() - before; got != 1 {
		t.Errorf("one update produced %d encodes, want 1: there is more than one frame loop", got)
	}
}

func TestASessionNeverAttachedIsReaped(t *testing.T) {
	// A create whose WebSocket upgrade never completes has no event loop, so
	// none of the timers that normally reap a session exist for it. Without the
	// attach deadline it holds a remote desktop and a slot until the process
	// restarts.
	remote := newFakeRemote()
	s := newSession("never", testConfig(), remote, options{
		reconnectGrace: 60 * time.Millisecond,
		now:            time.Now,
	})

	time.Sleep(20 * time.Millisecond)
	if remote.isClosed() {
		t.Fatal("the session was reaped before its grace elapsed")
	}
	waitFor(t, "the unattached session to be reaped", remote.isClosed)
	s.finish()
}

func TestAnAttachedSessionIsNotReapedByTheAttachDeadline(t *testing.T) {
	src := newFakeSource(64, 64)
	s, remote, _ := attachedReconnectSession(t, 80*time.Millisecond, src)
	time.Sleep(200 * time.Millisecond)

	if remote.isClosed() {
		t.Fatal("the attach deadline ended a session that was attached")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		t.Fatal("the session was shut down although it was attached")
	}
}

func TestWireFailureWhileAttachedDetachesRatherThanEndingTheSession(t *testing.T) {
	// A silent drop is usually noticed by the next write long before the read
	// deadline. Ending the session there would make the grace useless exactly
	// when it is needed, so a write failure has to detach.
	src := newFakeSource(64, 64)
	s, remote, _ := attachedReconnectSession(t, time.Minute, src)

	s.mu.Lock()
	tr := s.transport
	s.mu.Unlock()
	tr.(*fakeTransport).setSendErr(errors.New("broken pipe"))

	// The next frame's write fails.
	src.send(image.Rect(0, 0, 32, 32))
	waitFor(t, "the detach", s.isDetached)

	if remote.isClosed() {
		t.Fatal("a write failure ended the session instead of detaching it")
	}
}

func TestFrameThatFailsToSendAsksForTheWholeDesktop(t *testing.T) {
	// The accumulator is cleared before the frame is known to have gone out, and
	// the source only reports a region when it changes. A region that is
	// forgotten without being delivered is therefore never sent again: the
	// client keeps showing whatever happened to be there, which looks like a
	// block of an old window left behind and lasts until the page is reloaded.
	//
	// Asserted here rather than in a real session, because a real session shows
	// it only sometimes and only after a drag.
	src := newFakeSource(64, 64)
	s, _, _ := attachedReconnectSession(t, time.Minute, src)

	s.mu.Lock()
	tr := s.transport
	s.mu.Unlock()
	tr.(*fakeTransport).setSendErr(errors.New("broken pipe"))

	bounds := s.acc.Bounds()
	s.acc.Add(image.Rect(bounds.Min.X+8, bounds.Min.Y+8, bounds.Min.X+24, bounds.Min.Y+24))
	if s.acc.Empty() {
		t.Fatal("the region did not accumulate, so this proves nothing")
	}

	img := image.NewRGBA(bounds)
	if s.render(Update{Image: img}) {
		t.Fatal("render reported the session as healthy after a failed write")
	}

	rects, full := s.acc.Result()
	if !full {
		t.Fatalf("the accumulator asks for %d regions after a failed write, want the whole desktop", len(rects))
	}
}

func TestRepaintControlAsksForTheWholeDesktop(t *testing.T) {
	// The server sends a region when it changes and not before, so a client that
	// could not draw one has no other way to get it back: it would keep showing
	// whatever was there until the page was reloaded. This is the control that
	// lets it ask, and the frame that follows has to be the whole desktop.
	//
	// Asserted through the wire rather than by reading the session's flag: the
	// frame loop consumes that flag the moment it is set, which is the point of
	// it, so reading it would be a race rather than a check.
	src := newFakeSource(64, 64)
	_, _, tr := attachedReconnectSession(t, time.Minute, src)

	src.send(image.Rect(0, 0, 2, 2))
	waitFor(t, "a frame to be sent", func() bool { return len(tr.allFrames(t)) > 0 })
	before := len(tr.allFrames(t))

	tr.send(t, wsframe.SessionRepaint{})
	waitFor(t, "the whole desktop after asking for it", func() bool {
		for _, f := range tr.allFrames(t)[before:] {
			if f.IsFull() {
				return true
			}
		}
		return false
	})
}
