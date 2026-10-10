// Package rdp adapts a grdp client to what the gateway needs, and keeps grdp's
// types out of the rest of the program.
//
// It uses only what grdp already exposes, which since v0.8.0 includes the three
// things this package was waiting for: the framebuffer and its dirty
// rectangles, decoded pointer shapes, and Unicode key events. The requirements
// list in docs/grdp-requirements.md records what each one was and what is still
// open.
//
// # A credential leak this package works around
//
// grdp logs the account password in plaintext at INFO level during the CredSSP
// handshake:
//
//	protocol/nla/ntlm.go:442
//	glog.Infof("user: %s, passwd:%s", n.user, n.password)
//
// NewSetting defaults LogLevel to glog.INFO, and NewClient pushes that into the
// global logger, so the documented way to use grdp prints every remote
// password to stdout — which in a container is the container's log. See
// docs/grdp-requirements.md requirement H.
//
// Until that line is gone, this package pins the level at WARN and re-applies
// it after NewClient, because a credential must never reach a log. Debug
// logging can be enabled, and says loudly what it costs.
package rdp

import (
	"context"
	"errors"
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	"github.com/adfnekc/grdp/client"
	"github.com/adfnekc/grdp/glog"
	"github.com/adfnekc/grdp/protocol/pdu"
)

// Errors returned by this package.
var (
	// ErrClosed means the session is finished; input is a no-op.
	ErrClosed = errors.New("rdp: session is closed")
	// ErrUnknownKey means the code is not one this build can map to a scancode.
	ErrUnknownKey = errors.New("rdp: unmapped key code")
	// ErrGRDPPending means the method depends on a grdp change that has not
	// landed yet. The error text names the requirement.
	ErrGRDPPending = errors.New("rdp: blocked on a pending grdp change")
	// ErrNotReady means the session has not received its first frame, so there
	// is no desktop to read yet.
	ErrNotReady = errors.New("rdp: the session has no framebuffer yet")
	// ErrUnsupported means the method cannot work on this transport, such as
	// Unicode input over RFB.
	ErrUnsupported = errors.New("rdp: not supported by this transport")
)

// Error codes, matching the client's expectations in docs/…design.md §6.
const (
	CodeHostUnreachable = "host_unreachable"
	CodeTLSError        = "tls_error"
	CodeNLAFailed       = "nla_failed"
	CodeRDPAuthFailed   = "rdp_auth_failed"
	CodeProtocol        = "protocol_error"
	CodeUnsupported     = "unsupported"
	// CodeSessionReplaced means another connection took the session over. It is
	// worth its own code because the operator did nothing wrong and the fix is
	// to stop the other connection, not to retry.
	CodeSessionReplaced = "session_replaced"
)

// CodedError carries the machine-readable code alongside the cause, so the
// session layer does not have to re-inspect error text.
type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *CodedError) Unwrap() error { return e.Err }

// CodeOf returns the code carried by err, or "" if it has none.
func CodeOf(err error) string {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// Config describes one remote session.
type Config struct {
	// Host is "address:port".
	Host string
	User string
	// Password is the remote account password. It is held here for the life of
	// the session and must never be logged or written anywhere.
	Password string
	// Protocol selects the security path: "tls" (default), "nla" for CredSSP,
	// "rdp" for Standard RDP Security.
	Protocol string
	// Width and Height are the desktop size requested at connect time.
	Width, Height int
	// Name labels the session for the client UI.
	Name string
	// Timeout bounds the whole handshake. Zero means 30s.
	Timeout time.Duration
	// NoFastPathInput sends keyboard and mouse over the slow path instead of
	// the fast-path input PDU. It exists for diagnosing a server that ignores
	// one of the two; the fast path is the normal one.
	NoFastPathInput bool
	// Debug raises grdp's log level to INFO. It exists for diagnosing a
	// handshake, and it re-enables the credential logging described in the
	// package comment, so it is off by default and warns when used.
	Debug bool
	// TraceInput raises grdp's log level to TRACE, which prints the bytes of
	// every PDU it writes. That is the only way to tell an input the remote
	// ignored from an input that was never sent, so it exists for chasing
	// exactly that, and like Debug it re-enables the credential logging.
	TraceInput bool
}

func (c Config) validate() error {
	switch {
	case c.Host == "":
		return errors.New("rdp: Host is required")
	case c.Width <= 0 || c.Height <= 0:
		return errors.New("rdp: Width and Height must be positive")
	case c.Width > 0xffff || c.Height > 0xffff:
		return errors.New("rdp: Width and Height must fit the protocol's 16 bit fields")
	}
	switch c.Protocol {
	case "", "tls", "ssl", "nla", "hybrid", "credssp", "rdp", "standard":
		return nil
	default:
		return fmt.Errorf("rdp: unknown protocol %q", c.Protocol)
	}
}

// Event is something that happened to a session. Consumers switch on the
// concrete type.
type Event interface{ event() }

// Ready means the session is negotiated and the server is about to draw.
type Ready struct {
	Width    int
	Height   int
	Name     string
	Protocol string
}

func (Ready) event() {}

// ClipboardText is text the remote put on the shared clipboard.
//
// It is delivered as an event rather than written to any local clipboard: the
// browser keeps a log and copies on the operator's instruction, so a busy
// remote cannot overwrite what the operator is doing.
type ClipboardText struct {
	Text string
	At   time.Time
}

func (ClipboardText) event() {}

// CursorPos is the remote pointer position, in desktop pixels.
type CursorPos struct {
	X, Y int
}

func (CursorPos) event() {}

// ErrorEvent is a condition that ends or endangers the session. Err is always
// safe to show: it never contains a credential.
type ErrorEvent struct {
	Code string
	Err  error
}

func (ErrorEvent) event() {}

// Closed means the session is over and no further events will arrive.
type Closed struct {
	Reason string
	Err    error
}

func (Closed) event() {}

// Close reasons, matching the client's expectations.
const (
	ReasonUserRequested  = "user_requested"
	ReasonRemoteEnded    = "remote_ended"
	ReasonServerShutdown = "server_shutdown"
	ReasonError          = "error"
)

// unicodeSink is the part of a transport that sends Unicode key events.
//
// It is separate from inputSink because an RFB session has no such event, so
// asking for one there is a capability question rather than a bug.
type unicodeSink interface {
	UnicodeKeyDown(r rune)
	UnicodeKeyUp(r rune)
}

// textSink types a whole string, holding each key so a server that synthesises
// keystrokes from the events keeps all of them.
type textSink interface {
	TypeText(s string, hold time.Duration) error
}

// preambleSink sends the input sequence mstsc sends once the session is ready.
//
// It is separate from inputSink because it is a thing to do at the right moment
// rather than a way to drive a session, and because a protocol without such a
// sequence should not have to pretend it has one.
type preambleSink interface {
	SendInputPreamble(toggleFlags uint32) error
}

// inputSink is the part of grdp's client this package drives.
//
// It exists so the input path can be tested without a server: the real
// *client.Client satisfies it, and so does a recorder in the tests.
//
// The handler-registration methods are not here, because they are not part of
// driving a session and only the concrete client has them.
type inputSink interface {
	KeyDown(sc int, name string)
	KeyUp(sc int, name string)
	MouseMove(x, y int)
	MouseDown(button, x, y int)
	MouseUp(button, x, y int)
	MouseWheel(scroll, x, y int)
	SetClipboardText(text string) error
	RequestClipboardText() error
	Close()
}

// Session is one connected remote desktop.
//
// Input methods are safe to call concurrently and become no-ops after Close.
type Session struct {
	cfg Config

	// sink is the input path, behind an interface so it can be driven by a
	// recorder in the tests.
	sink inputSink
	// cli is the concrete client, for the capabilities that only it has:
	// the framebuffer, pointer shapes, resizing. Those need a server to be
	// meaningful, so they are not abstracted.
	cli *client.Client

	// done is closed once, by Close. It stops the pump and tells late event
	// producers to drop rather than block.
	done      chan struct{}
	closeOnce sync.Once

	// pumpOnce guards the single reader of queue. Two pumps would both close
	// events, which panics, so starting one is made idempotent rather than
	// relying on every construction path remembering not to start a second.
	pumpOnce sync.Once

	// queue is written by grdp's callbacks and drained by the pump. It is never
	// closed, so a callback that fires during shutdown cannot panic on a send.
	queue   chan Event
	events  chan Event
	dropped atomicCounter
}

// newSession wires a session around an input sink and starts its pump.
func newSession(cfg Config, sink inputSink) *Session {
	s := &Session{
		cfg:    cfg,
		sink:   sink,
		done:   make(chan struct{}),
		queue:  make(chan Event, 256),
		events: make(chan Event, 64),
	}
	s.start()
	return s
}

// start launches the event pump, at most once per session.
func (s *Session) start() {
	s.pumpOnce.Do(func() { go s.pump() })
}

// Dial connects and completes the handshake.
//
// The password is copied into the grdp client, which keeps it for the life of
// the connection; nothing here writes it anywhere else.
func Dial(ctx context.Context, cfg Config) (*Session, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}

	level := effectiveLogLevel(cfg)
	if level <= glog.INFO {
		glog.Warn("rdp: debug logging enabled; grdp logs the account password in plaintext at this level")
	}

	setting := client.NewSetting()
	setting.Width = cfg.Width
	setting.Height = cfg.Height
	setting.Protocol = cfg.Protocol
	setting.Timeout = cfg.Timeout
	setting.EnableClipboard = true
	// Display Control is what makes RequestResize work: without the channel the
	// server has no way to be asked, and the call fails rather than silently
	// doing nothing.
	setting.EnableDisplayControl = true
	setting.NoFastPathInput = cfg.NoFastPathInput
	// EnableOrders and EnableEGFX stay off: both change how the server draws,
	// and with the frame path not wired up yet there is nothing to benefit.
	setting.LogLevel = level

	c := client.NewClient(cfg.Host, cfg.User, cfg.Password, client.TC_RDP, setting)
	// NewClient pushes setting.LogLevel into the process-global logger.
	// Re-assert it so a future grdp that ignores the setting, or another
	// caller that changed the level, cannot re-enable credential logging.
	glog.SetLevel(level)

	s := newSession(cfg, c)
	s.cli = c

	// Handlers are registered before Login so no event can be missed.
	c.OnReady(func() {
		s.sendPreamble()
		s.emit(Ready{Width: cfg.Width, Height: cfg.Height, Name: cfg.Name, Protocol: cfg.Protocol})
	})
	c.OnClipboardText(func(text string) {
		s.emit(ClipboardText{Text: text, At: time.Now()})
	})
	c.OnPointerPosition(func(x, y int) {
		s.emit(CursorPos{X: x, Y: y})
	})
	c.OnError(func(err error) {
		s.emit(ErrorEvent{Code: codeFor(err), Err: redact(err, cfg.Password)})
	})
	c.OnClose(func() {
		s.emit(Closed{Reason: ReasonRemoteEnded})
	})

	// The pump is already running; it is the only closer of events.
	if err := c.LoginContext(ctx); err != nil {
		s.Close()
		return nil, &CodedError{Code: codeFor(err), Err: redact(err, cfg.Password)}
	}
	return s, nil
}

// Events is the stream of session events. It is closed after Closed is
// delivered, so a range loop terminates on its own.
func (s *Session) Events() <-chan Event { return s.events }

// Name is the label this session was configured with.
func (s *Session) Name() string { return s.cfg.Name }

// Host is the remote address this session was configured with.
func (s *Session) Host() string { return s.cfg.Host }

// DroppedEvents is how many events were discarded because the consumer could
// not keep up. It is reported rather than hidden: a non-zero value means the
// picture or the clipboard is stale.
func (s *Session) DroppedEvents() uint64 { return s.dropped.load() }

// Close ends the session. It is safe to call more than once.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.sink.Close()
	})
}

// emit queues an event for the pump, dropping it if the consumer has fallen
// too far behind. Dropping is deliberate: blocking here would stall grdp's
// decode loop, which is worse than a stale frame.
func (s *Session) emit(ev Event) {
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.queue <- ev:
	default:
		s.dropped.inc()
	}
}

// pump is the only writer to events, so it is the only thing that closes it.
//
// On shutdown it drains what is already queued before stopping. Without that,
// a Select between a ready queue and a closed done would pick at random, and the
// event emitted immediately before Close — usually the one that says why the
// session ended — would be lost about half the time.
func (s *Session) pump() {
	defer close(s.events)
	for {
		select {
		case ev := <-s.queue:
			if !s.forward(ev) {
				s.drain()
				return
			}
		case <-s.done:
			s.drain()
			return
		}
	}
}

// drain delivers whatever is already queued, without waiting for the consumer.
//
// It never blocks: a consumer that has stopped reading must not keep a
// goroutine alive, and the queue may legitimately hold more than the output
// buffer can take.
func (s *Session) drain() {
	for {
		select {
		case ev := <-s.queue:
			select {
			case s.events <- ev:
			default:
				s.dropped.inc()
			}
		default:
			return
		}
	}
}

// forward passes an event to the consumer, waiting for room. It reports false
// once the session is shutting down, after making one last attempt that does
// not wait, so an event already in flight is delivered when the consumer has
// room for it.
func (s *Session) forward(ev Event) bool {
	select {
	case s.events <- ev:
		return true
	case <-s.done:
		select {
		case s.events <- ev:
		default:
			s.dropped.inc()
		}
		return false
	}
}

// ---------------------------------------------------------------- input

// Key presses or releases the key the browser reported, identified by its
// KeyboardEvent.code.
//
// It reports ErrUnknownKey for a code this build cannot map, rather than
// silently swallowing the keystroke.
func (s *Session) Key(code string, down bool) error {
	sc, ok := ScancodeForCode(code)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKey, code)
	}
	s.KeyScancode(sc, down)
	return nil
}

// KeyScancode presses or releases a raw PC set 1 scancode. A value in
// 0xE000..0xE0FF is sent as an extended key, which is what grdp expects.
func (s *Session) KeyScancode(sc uint16, down bool) {
	if !s.live() {
		return
	}
	// grdp ignores name; the scancode is what reaches the server.
	if down {
		s.sink.KeyDown(int(sc), "")
	} else {
		s.sink.KeyUp(int(sc), "")
	}
}

// UnicodeKey presses and releases one character using the RDP Unicode keyboard
// event, which is the only way to deliver what an input method composed: a
// scancode cannot express a character the layout did not produce.
//
// It reports ErrClosed once the session is over. A character the protocol
// cannot carry — a lone surrogate half — is dropped by the library rather than
// sent as half a pair.
func (s *Session) UnicodeKey(r rune) error {
	if !s.live() {
		return ErrClosed
	}
	us, ok := s.sink.(unicodeSink)
	if !ok {
		return fmt.Errorf("%w: this transport has no Unicode key event", ErrUnsupported)
	}
	us.UnicodeKeyDown(r)
	us.UnicodeKeyUp(r)
	return nil
}

// TypeText types a string, holding each key briefly so a server that synthesises
// keystrokes from the events rather than reading a queue keeps all of them.
func (s *Session) TypeText(text string) error {
	if !s.live() {
		return ErrClosed
	}
	ts, ok := s.sink.(textSink)
	if !ok {
		return fmt.Errorf("%w: this transport cannot type text", ErrUnsupported)
	}
	return ts.TypeText(text, unicodeHold)
}

// unicodeHold is how long each key is held. grdp documents this as the
// difference between the whole string arriving and only the first character
// arriving on a server that has to synthesise keystrokes.
const unicodeHold = 5 * time.Millisecond

// MouseMove moves the pointer to a point in remote desktop pixels.
func (s *Session) MouseMove(x, y int) {
	if !s.live() {
		return
	}
	s.sink.MouseMove(x, y)
}

// MouseButton presses or releases a pointer button at a point in remote
// desktop pixels. Button follows grdp's sender-side convention: 0 is left,
// 1 is middle, 2 is right.
func (s *Session) MouseButton(button int, down bool, x, y int) {
	if !s.live() {
		return
	}
	if down {
		s.sink.MouseDown(button, x, y)
	} else {
		s.sink.MouseUp(button, x, y)
	}
}

// MouseWheel turns the wheel at a point. Delta is in notches and positive means
// away from the user.
//
// The wire field counts in units of 120 degrees, so a notch is 120 and not 1.
// The library clamps to the nine bit field the field actually is, which is two
// notches, so a larger delta is sent as several messages rather than being
// silently cut down to two. Messages are sent separately because the field has
// no room for the total, not because the server needs them one at a time.
func (s *Session) MouseWheel(delta, x, y int) {
	if !s.live() {
		return
	}
	for delta != 0 {
		step := delta
		if step > maxWheelNotches {
			step = maxWheelNotches
		} else if step < -maxWheelNotches {
			step = -maxWheelNotches
		}
		s.sink.MouseWheel(step*wheelUnitsPerNotch, x, y)
		delta -= step
	}
}

const (
	// wheelUnitsPerNotch is 120 degrees, the unit the field is documented in.
	wheelUnitsPerNotch = 120
	// maxWheelNotches is the largest whole number of notches that fits the nine
	// bit field, which is what the library clamps to.
	maxWheelNotches = 2
)

// sendPreamble sends the input sequence mstsc sends once the session is ready: a
// Tab release, the toggle key state, then another Tab release. Both mstsc and
// FreeRDP send it, and a server that never sees it does not take pointer button
// input at all, which looks exactly like the mouse being broken.
//
// The toggle state is sent as all-off. Reading the remote's Caps Lock and Num
// Lock state would mean parsing it back out of the keyboard state the server
// reports, and getting it wrong is worse than starting from a known state: the
// user can press the key once to correct it.
func (s *Session) sendPreamble() {
	p, ok := s.sink.(preambleSink)
	if !ok {
		return
	}
	if err := p.SendInputPreamble(0); err != nil {
		// Worth saying, because a server that never gets this sequence does not
		// take pointer buttons at all, and that looks like a broken mouse rather
		// than a missing handshake. It does not end the session: typing and
		// pointer movement still work.
		s.emit(ErrorEvent{Code: CodeUnsupported, Err: fmt.Errorf(
			"the remote did not get the input sequence other clients start with, "+
				"so pointer buttons may do nothing: %w", err)})
	}
}

// SendCtrlAltEnd sends Ctrl+Alt+End, which is the secure attention sequence a
// remote session answers to. A browser cannot send Ctrl+Alt+Del — the operating
// system claims it before the page sees it — so the UI offers this instead.
func (s *Session) SendCtrlAltEnd() {
	s.KeyScancode(ScControlLeft, true)
	s.KeyScancode(ScAltLeft, true)
	s.KeyScancode(ScDelete, true)
	s.KeyScancode(ScDelete, false)
	s.KeyScancode(ScAltLeft, false)
	s.KeyScancode(ScControlLeft, false)
}

// ---------------------------------------------------------------- clipboard

// SetClipboardText publishes text on the shared clipboard so a paste on the
// remote pastes it.
func (s *Session) SetClipboardText(text string) error {
	if !s.live() {
		return ErrClosed
	}
	return s.sink.SetClipboardText(text)
}

// RequestClipboardText asks the remote for the text it last announced. A
// server can announce a format before it can serve it, in which case the
// automatic fetch fails and an explicit request later is what gets the data.
func (s *Session) RequestClipboardText() error {
	if !s.live() {
		return ErrClosed
	}
	return s.sink.RequestClipboardText()
}

// ---------------------------------------------------------------- frames

// Framebuffer is the composited desktop the encoder reads from.
type Framebuffer interface {
	// Bounds is the desktop size. A zero rectangle means there is nothing to
	// read yet.
	Bounds() image.Rectangle
	// Pix is the pixel data, BGRA with the first byte blue, top row first.
	//
	// It is the live buffer, not a copy, and it stays valid only until the
	// next OnFrame callback returns. A caller that encodes later has to copy
	// the regions it cares about before then, which is what the session's
	// frame source does.
	Pix() []byte
	// Stride is the byte length of one row.
	Stride() int
}

// Cursor is the pointer as the server wants it drawn.
//
// The pointer is not part of the desktop image: it sits over it at a position
// the server reports separately, which is why a bitmap update never contains it.
type Cursor struct {
	// Image carries alpha, so the shape composites over the desktop.
	Image *image.RGBA
	// Hotspot is the point inside Image that sits on the reported position.
	Hotspot image.Point
	// System is true when the server named one of its own cursors rather than
	// sending pixels. Image is nil, and the caller substitutes its own artwork.
	System bool
	// SystemType is the server's raw system pointer type.
	SystemType uint32
	// RawXorLen and RawAndLen are how many bytes the masks the shape arrived in
	// actually held. They are here because a shape that decodes to almost
	// nothing is either a shape that is almost nothing or a shape whose masks
	// were shorter than the declared depth expected, and only these tell the two
	// apart.
	RawXorLen int
	RawAndLen int
	// XorBpp is the depth the shape was decoded at: one for a two colour
	// pointer, three or four for a colour one. A two colour pointer is the one
	// that carries the invert pixels, which cannot be represented as a colour,
	// so a caller that wants to know why a shape looks odd needs this.
	XorBpp int
}

// Hidden reports whether the server asked for no pointer at all.
func (c Cursor) Hidden() bool { return hiddenCursorType(c.System, c.SystemType) }

// Framebuffer returns the buffer every decoded frame lands in.
//
// It reports ErrNotReady before the first update, because there is no desktop
// to read until then.
func (s *Session) Framebuffer() (Framebuffer, error) {
	if s.cli == nil {
		return nil, fmt.Errorf("%w: this session has no grdp client", ErrUnsupported)
	}
	fb := s.cli.Framebuffer()
	if fb == nil || fb.Bounds().Empty() {
		return nil, ErrNotReady
	}
	return fb, nil
}

// OnFrame subscribes to desktop updates.
//
// The callback receives one batch per update, with the rectangles already
// merged and clipped to the framebuffer. The pixels it reads from Framebuffer
// are only valid until the callback returns.
func (s *Session) OnFrame(f func(dirty []image.Rectangle)) error {
	if s.cli == nil {
		return fmt.Errorf("%w: this session has no grdp client", ErrUnsupported)
	}
	s.cli.OnFrame(f)
	return nil
}

// OnCursor subscribes to pointer shape changes.
func (s *Session) OnCursor(f func(Cursor)) error {
	if s.cli == nil {
		return fmt.Errorf("%w: this session has no grdp client", ErrUnsupported)
	}
	s.cli.OnCursor(func(cur *client.Cursor) {
		if cur == nil {
			return
		}
		f(Cursor{
			Image:      cur.Image,
			Hotspot:    cur.Hotspot,
			System:     cur.System,
			SystemType: cur.SystemType,
			XorBpp:     cur.XorBpp,
			RawXorLen:  len(cur.Xor),
			RawAndLen:  len(cur.And),
		})
	})
	return nil
}

// OnCursorPos subscribes to the server's pointer position, in desktop pixels.
func (s *Session) OnCursorPos(f func(x, y int)) error {
	if s.cli == nil {
		return fmt.Errorf("%w: this session has no grdp client", ErrUnsupported)
	}
	s.cli.OnCursorPos(f)
	return nil
}

// RequestResize asks the server for a different desktop size.
//
// The server rounds to a mode it has, so the size that takes effect is the one
// OnResize reports, which may differ from what was asked for.
func (s *Session) RequestResize(w, h int) error {
	if s.cli == nil {
		return fmt.Errorf("%w: this session has no grdp client", ErrUnsupported)
	}
	return s.cli.RequestResize(w, h)
}

// OnResize subscribes to the desktop size actually in effect.
func (s *Session) OnResize(f func(w, h int)) error {
	if s.cli == nil {
		return fmt.Errorf("%w: this session has no grdp client", ErrUnsupported)
	}
	s.cli.OnResize(f)
	return nil
}

// ---------------------------------------------------------------- internals// ---------------------------------------------------------------- internals

// hiddenCursorType reports whether a system cursor means "draw nothing".
// pdu.SYSPTR_NULL is the only value the specification names for that.
func hiddenCursorType(system bool, t uint32) bool {
	return system && t == pdu.SYSPTR_NULL
}

func (s *Session) live() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// effectiveLogLevel pins grdp's logger at WARN unless the operator explicitly
// asked for debug output, because grdp logs the account password at INFO.
func effectiveLogLevel(cfg Config) glog.LEVEL {
	switch {
	case cfg.TraceInput:
		return glog.TRACE
	case cfg.Debug:
		return glog.INFO
	}
	return glog.WARN
}

// codeFor classifies a grdp error into one of the codes the client knows how to
// explain.
//
// The sentinels come first, and since v0.8.1 they are the whole mechanism: grdp
// now classifies every login failure centrally, so an error that reaches here
// from Login always carries one.
//
// The text matching after them is a fallback for an error that arrived by some
// other route, or from something that wrapped a grdp error without keeping the
// chain. It is deliberately last, and an unrecognised failure is reported as
// protocol_error rather than guessed at.
func codeFor(err error) string {
	if err == nil {
		return ""
	}

	// The sentinels grdp exports are the way this is meant to work: a type test
	// cannot be broken by a change of wording. Since v0.8.1 grdp classifies
	// every login failure centrally, so these five cover the whole surface and
	// the text matching below is a fallback rather than the mechanism.
	switch {
	case errors.Is(err, client.ErrUnreachable):
		return CodeHostUnreachable
	case errors.Is(err, client.ErrTLSFailure):
		return CodeTLSError
	case errors.Is(err, client.ErrCredSSP):
		return CodeNLAFailed
	case errors.Is(err, client.ErrAuthenticationFailed):
		return CodeRDPAuthFailed
	case errors.Is(err, client.ErrSessionEndedByServer):
		return CodeSessionReplaced
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return CodeHostUnreachable
	}

	msg := strings.ToLower(err.Error())

	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return CodeHostUnreachable

	case containsAny(msg,
		// The dial itself never completed, or the peer went away mid-handshake.
		// A reset during CredSSP is a common way for NLA to fail and is
		// indistinguishable from an unreachable host at this layer.
		"cannot reach the server", "dial tcp", "dial err",
		"connection refused", "no route to host", "network is unreachable",
		"i/o timeout", "connection reset", ": eof", "unexpected eof",
		// Some paths surface a timeout as text rather than as the sentinel.
		"context deadline", "deadline exceeded"):
		return CodeHostUnreachable

	case containsAny(msg, "tls", "certificate", "x509"):
		return CodeTLSError

	case containsAny(msg, "credssp", "ntlm", "nla", "premaster", "server challenge", "signing key"):
		return CodeNLAFailed

	case containsAny(msg, "logon", "access denied", "bad credentials", "wrong password", "invalid password"):
		return CodeRDPAuthFailed

	default:
		return CodeProtocol
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// redact removes the password from an error before it can travel any further.
//
// It should never have anything to do, but an error is the one place a
// credential reaches a log line, a JSON field and an operator's screen, so it is
// scrubbed at the boundary rather than trusted.
//
// When there is nothing to scrub the error is returned unchanged, because
// wrapping it in a fresh one would drop the chain — and the chain is what lets
// http and session call errors.Is on grdp's sentinels. Only the case that
// actually contains a credential gives that up, where losing the sentinel is the
// smaller loss and the code has already been derived from the original.
func redact(err error, password string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !containsToken(msg, password) {
		return err
	}
	return errors.New(replaceTokens(msg, password, "[redacted]"))
}

// containsToken reports whether token appears in msg as a whole word.
//
// Loose substring matching is what a first attempt did, and a one-character
// password turned "dial tcp" into "dial tc[redacted]": it mangled the message
// and, worse, threw away the error's chain for a coincidence, which silently
// undid the sentinels for everything above this package. A credential that
// really leaked appears as a field or a standalone word, not as a letter inside
// another word.
func containsToken(msg, token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(msg); {
		j := strings.Index(msg[i:], token)
		if j < 0 {
			return false
		}
		start := i + j
		if tokenBoundary(msg, start, start+len(token)) {
			return true
		}
		i = start + 1
	}
	return false
}

// replaceTokens replaces every whole-word occurrence, leaving coincidental
// substrings alone.
func replaceTokens(msg, token, repl string) string {
	var b strings.Builder
	for i := 0; i < len(msg); {
		j := strings.Index(msg[i:], token)
		if j < 0 {
			b.WriteString(msg[i:])
			return b.String()
		}
		start := i + j
		end := start + len(token)
		b.WriteString(msg[i:start])
		if tokenBoundary(msg, start, end) {
			b.WriteString(repl)
		} else {
			b.WriteString(msg[start:end])
		}
		i = end
	}
	return b.String()
}

// tokenBoundary reports whether [start,end) is delimited rather than sitting
// inside a longer word.
func tokenBoundary(msg string, start, end int) bool {
	if start > 0 && isWordByte(msg[start-1]) {
		return false
	}
	if end < len(msg) && isWordByte(msg[end]) {
		return false
	}
	return true
}

func isWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_':
		return true
	default:
		return false
	}
}

// atomicCounter is a tiny counter kept separate so Session has no dependency on
// a particular atomic type.
type atomicCounter struct {
	mu sync.Mutex
	n  uint64
}

func (c *atomicCounter) inc() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *atomicCounter) load() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
