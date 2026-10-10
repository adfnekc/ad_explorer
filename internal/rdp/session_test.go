package rdp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adfnekc/grdp/client"
	"github.com/adfnekc/grdp/glog"
)

// fakeSink records everything a session sends to its client, so the input path
// can be checked without a server.
type fakeSink struct {
	mu     sync.Mutex
	calls  []string
	clip   []string
	closed bool
}

func (f *fakeSink) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeSink) KeyDown(sc int, _ string) { f.record("down %#x", sc) }
func (f *fakeSink) KeyUp(sc int, _ string)   { f.record("up %#x", sc) }
func (f *fakeSink) MouseMove(x, y int)       { f.record("move %d %d", x, y) }
func (f *fakeSink) MouseDown(b, x, y int)    { f.record("btn%d down %d %d", b, x, y) }
func (f *fakeSink) MouseUp(b, x, y int)      { f.record("btn%d up %d %d", b, x, y) }
func (f *fakeSink) MouseWheel(d, x, y int)   { f.record("wheel %d %d %d", d, x, y) }

func (f *fakeSink) SetClipboardText(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clip = append(f.clip, text)
	return nil
}

func (f *fakeSink) RequestClipboardText() error {
	f.record("clipboard.request")
	return nil
}

func (f *fakeSink) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *fakeSink) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSink) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func newTestSession() (*Session, *fakeSink) {
	sink := &fakeSink{}
	s := newSession(Config{Host: "10.0.0.1:3389", Name: "test", Width: 1280, Height: 720}, sink)
	return s, sink
}

// The rotation field is nine bits of two's complement and a notch is 120, so it
// holds two notches. More than that has to be sent as several messages: cutting
// it down to two would make a fast scroll move a third as far as it should,
// which reads as the wheel being insensitive rather than as a lost message.
func TestWheelSplitsToFitTheField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta int
		want  []string
	}{
		{"one notch", 1, []string{"wheel 120 7 8"}},
		{"negative", -1, []string{"wheel -120 7 8"}},
		{"the most that fits", 2, []string{"wheel 240 7 8"}},
		{"one past that", 3, []string{"wheel 240 7 8", "wheel 120 7 8"}},
		{"negative one past", -3, []string{"wheel -240 7 8", "wheel -120 7 8"}},
		{"seven", 7, []string{
			"wheel 240 7 8", "wheel 240 7 8", "wheel 240 7 8", "wheel 120 7 8"}},
		{"nothing", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sink := newTestSession()
			defer s.Close()
			s.MouseWheel(tc.delta, 7, 8)
			var got []string
			for _, c := range sink.calls {
				if strings.HasPrefix(c, "wheel") {
					got = append(got, c)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("wheel %d: got %v, want %v", tc.delta, got, tc.want)
			}
		})
	}
}

func TestSessionInputRouting(t *testing.T) {
	s, sink := newTestSession()
	defer s.Close()

	if err := s.Key("KeyA", true); err != nil {
		t.Fatalf("Key(KeyA, down): %v", err)
	}
	if err := s.Key("KeyA", false); err != nil {
		t.Fatalf("Key(KeyA, up): %v", err)
	}
	s.MouseMove(100, 200)
	s.MouseButton(0, true, 100, 200)
	s.MouseButton(0, false, 100, 200)
	s.MouseButton(2, true, 5, 6)
	// Three notches, which is more than the field holds, so it goes as two.
	s.MouseWheel(-3, 7, 8)

	want := []string{
		"down 0x1e", // KeyA
		"up 0x1e",
		"move 100 200",
		"btn0 down 100 200",
		"btn0 up 100 200",
		"btn2 down 5 6",
		"wheel -240 7 8",
		"wheel -120 7 8",
	}
	got := sink.got()
	if len(got) != len(want) {
		t.Fatalf("recorded %d calls %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSessionKeyRejectsUnmappedCode(t *testing.T) {
	s, sink := newTestSession()
	defer s.Close()

	err := s.Key("Pause", true)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Key(Pause) = %v, want ErrUnknownKey", err)
	}
	if calls := sink.got(); len(calls) != 0 {
		t.Errorf("an unmapped key still reached the client: %v", calls)
	}
}

func TestSessionExtendedKeyCarriesPrefix(t *testing.T) {
	s, sink := newTestSession()
	defer s.Close()

	if err := s.Key("ArrowUp", true); err != nil {
		t.Fatalf("Key(ArrowUp): %v", err)
	}
	got := sink.got()
	if len(got) != 1 || got[0] != "down 0xe048" {
		t.Errorf("recorded %v, want [down 0xe048]", got)
	}
}

func TestSessionSendCtrlAltEndSequence(t *testing.T) {
	s, sink := newTestSession()
	defer s.Close()

	s.SendCtrlAltEnd()

	// Ctrl and Alt go down before Delete, and are released in reverse, which
	// is what a real keyboard does. Release order matters: a server that sees
	// Delete released before Alt can leave the modifier stuck.
	want := []string{
		"down 0x1d",   // Ctrl
		"down 0x38",   // Alt
		"down 0xe053", // Delete, extended
		"up 0xe053",
		"up 0x38",
		"up 0x1d",
	}
	got := sink.got()
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSessionClipboard(t *testing.T) {
	s, sink := newTestSession()
	defer s.Close()

	if err := s.SetClipboardText("hello 中文"); err != nil {
		t.Fatalf("SetClipboardText: %v", err)
	}
	if err := s.RequestClipboardText(); err != nil {
		t.Fatalf("RequestClipboardText: %v", err)
	}
	if len(sink.clip) != 1 || sink.clip[0] != "hello 中文" {
		t.Errorf("clipboard = %v, want [hello 中文]", sink.clip)
	}
}

func TestSessionInputIsNoOpAfterClose(t *testing.T) {
	s, sink := newTestSession()
	s.Close()
	s.Close() // must be safe to call twice

	s.KeyScancode(0x1E, true)
	s.MouseMove(1, 2)
	s.MouseButton(0, true, 1, 2)
	s.MouseWheel(1, 1, 1)

	if calls := sink.got(); len(calls) != 0 {
		t.Errorf("input after Close reached the client: %v", calls)
	}
	if err := s.SetClipboardText("x"); !errors.Is(err, ErrClosed) {
		t.Errorf("SetClipboardText after Close = %v, want ErrClosed", err)
	}
	if err := s.RequestClipboardText(); !errors.Is(err, ErrClosed) {
		t.Errorf("RequestClipboardText after Close = %v, want ErrClosed", err)
	}
	if !sink.isClosed() {
		t.Error("Close did not close the underlying sink")
	}
}

func TestSessionEventsCloseAfterShutdown(t *testing.T) {
	s, _ := newTestSession()
	s.emit(Ready{Width: 1280, Height: 720, Name: "test"})
	s.Close()

	// A range loop over Events must terminate; that is what lets the session
	// layer wait for delivery without a separate signal.
	deadline := time.After(3 * time.Second)
	sawReady := false
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				if !sawReady {
					t.Error("events closed before the queued Ready was delivered")
				}
				return
			}
			if _, isReady := ev.(Ready); isReady {
				sawReady = true
			}
		case <-deadline:
			t.Fatal("Events() did not close after Close()")
		}
	}
}

func TestSessionDropsEventsInsteadOfBlocking(t *testing.T) {
	// The pump is deliberately stopped from draining so the queue overflows,
	// which is what happens when a consumer stalls. Nothing may block: grdp's
	// decode loop is on the other side of emit.
	s := &Session{
		cfg:    Config{},
		sink:   &fakeSink{},
		done:   make(chan struct{}),
		queue:  make(chan Event, 1),
		events: make(chan Event, 1),
	}
	// No pump goroutine: the listener here never reads events.

	for i := 0; i < 50; i++ {
		done := make(chan struct{})
		go func() {
			s.emit(ClipboardText{Text: "x"})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("emit blocked when the queue was full")
		}
	}
	if s.DroppedEvents() == 0 {
		t.Error("DroppedEvents() = 0 although the queue overflowed; the count is what tells the operator the view is stale")
	}
}

func TestSessionEmitsNothingAfterClose(t *testing.T) {
	s, _ := newTestSession()
	s.Close()
	before := s.DroppedEvents()
	s.emit(ClipboardText{Text: "late"})
	if s.DroppedEvents() != before {
		t.Error("emit after Close was counted as a drop; it should be ignored")
	}
}

func TestConfigValidate(t *testing.T) {
	base := Config{Host: "10.0.0.1:3389", Width: 1280, Height: 720}

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"ok", func(*Config) {}, false},
		{"ok explicit tls", func(c *Config) { c.Protocol = "tls" }, false},
		{"ok nla", func(c *Config) { c.Protocol = "nla" }, false},
		{"ok standard rdp", func(c *Config) { c.Protocol = "rdp" }, false},
		{"missing host", func(c *Config) { c.Host = "" }, true},
		{"zero width", func(c *Config) { c.Width = 0 }, true},
		{"negative height", func(c *Config) { c.Height = -1 }, true},
		{"too wide", func(c *Config) { c.Width = 0x10000 }, true},
		{"unknown protocol", func(c *Config) { c.Protocol = "kerberos" }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			err := cfg.validate()
			if tc.wantErr && err == nil {
				t.Error("validate() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validate() = %v, want nil", err)
			}
		})
	}
}

func TestDialRejectsBadConfigBeforeTouchingTheNetwork(t *testing.T) {
	// A bad config must not reach grdp, so this returns without a server and
	// without a dial timeout.
	_, err := Dial(context.Background(), Config{Host: "", Width: 1280, Height: 720})
	if err == nil {
		t.Fatal("Dial accepted an empty host")
	}
	if CodeOf(err) != "" {
		t.Errorf("CodeOf(config error) = %q, want empty: a config error is not a session error", CodeOf(err))
	}
}

func TestEffectiveLogLevelNeverEnablesCredentialLoggingByAccident(t *testing.T) {
	// grdp logs "user: %s, passwd:%s" at INFO (protocol/nla/ntlm.go:442). The
	// default must not be INFO, or every NLA connect writes the password to
	// stdout.
	if got := effectiveLogLevel(Config{}); got != glog.WARN {
		t.Errorf("effectiveLogLevel(default) = %v, want WARN", got)
	}
	if got := effectiveLogLevel(Config{}); got == glog.INFO || got == glog.DEBUG {
		t.Error("the default log level reaches grdp's credential log line")
	}
	// Debug is an explicit opt-in and is allowed to reach INFO.
	if got := effectiveLogLevel(Config{Debug: true}); got != glog.INFO {
		t.Errorf("effectiveLogLevel(Debug) = %v, want INFO", got)
	}
}

func TestCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"dial", errors.New("[dial err] dial tcp 10.0.0.1:3389: connect: connection refused"), CodeHostUnreachable},
		{"no route", errors.New("dial tcp: no route to host"), CodeHostUnreachable},
		{"timeout", errors.New("dial tcp: i/o timeout"), CodeHostUnreachable},
		{"deadline word only", errors.New("dial tcp: context deadline exceeded"), CodeHostUnreachable},
		{"context deadline", context.DeadlineExceeded, CodeHostUnreachable},
		{"wrapped deadline", fmt.Errorf("client: session not ready: %w", context.DeadlineExceeded), CodeHostUnreachable},
		{"tls", errors.New("sec: server certificate has no public key"), CodeTLSError},
		{"x509", errors.New("x509: certificate signed by unknown authority"), CodeTLSError},
		{"credssp", errors.New("nla: credssp: NTLM negotiate failed"), CodeNLAFailed},
		{"premaster", errors.New("sec: encrypt premaster secret err: short read"), CodeNLAFailed},
		{"denied", errors.New("logon failed: access denied"), CodeRDPAuthFailed},
		{"unknown", errors.New("sec: security exchange carried no security data"), CodeProtocol},

		// The sentinels grdp v0.8.0 exports. These are checked before any text
		// is matched, so a rewording cannot move them.
		{"sentinel: unreachable", client.ErrUnreachable, CodeHostUnreachable},
		{"sentinel: TLS", client.ErrTLSFailure, CodeTLSError},
		{"sentinel: CredSSP", client.ErrCredSSP, CodeNLAFailed},
		{"sentinel: rejected credentials", client.ErrAuthenticationFailed, CodeRDPAuthFailed},
		{"sentinel: session taken over", client.ErrSessionEndedByServer, CodeSessionReplaced},
		{"sentinel wrapped", fmt.Errorf("login: %w", client.ErrAuthenticationFailed), CodeRDPAuthFailed},
		{"sentinel wrapped twice", fmt.Errorf("session: %w",
			fmt.Errorf("dial: %w", client.ErrTLSFailure)), CodeTLSError},

		// The shapes grdp v0.8.0 actually produced against a real Windows host,
		// copied from a run rather than invented.
		{"grpd: unreachable, new wording",
			errors.New("client: cannot reach the server: dial tcp 192.168.75.130:3389: i/o timeout"),
			CodeHostUnreachable},
		{"grpd: server requires NLA",
			errors.New("client: the RDP connection failed: x224: RDP security negotiation failed (0x00000005): the server requires CredSSP (NLA)"),
			CodeNLAFailed},
		{"grpd: reset during handshake",
			errors.New("client: the RDP connection failed: x224: transport closed during handshake: read tcp 172.18.226.39:51412->192.168.75.130:3389: read: connection reset by peer"),
			CodeHostUnreachable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codeFor(tc.err); got != tc.want {
				t.Errorf("codeFor(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestCodedError(t *testing.T) {
	inner := errors.New("connection refused")
	err := error(&CodedError{Code: CodeHostUnreachable, Err: inner})

	if CodeOf(err) != CodeHostUnreachable {
		t.Errorf("CodeOf = %q, want %q", CodeOf(err), CodeHostUnreachable)
	}
	if !errors.Is(err, inner) {
		t.Error("CodedError does not unwrap to its cause")
	}
	if CodeOf(inner) != "" {
		t.Error("CodeOf on a plain error should be empty")
	}
	if strings.Contains(err.Error(), "password") {
		t.Errorf("error text looks like it carries a credential: %q", err)
	}
}

func TestRedactRemovesPassword(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		password string
		want     string
	}{
		{"nil", nil, "s3cret", ""},
		{"password present", errors.New("login failed for user: admin, passwd:s3cret"), "s3cret", "[redacted]"},
		{"password appears twice", errors.New("s3cret then s3cret again"), "s3cret", "[redacted]"},
		{"unrelated error", errors.New("connection refused"), "s3cret", "connection refused"},
		{"empty password", errors.New("anything"), "", "anything"},
		{"password empty in text", errors.New("no credential here"), "s3cret", "no credential here"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redact(tc.err, tc.password)
			if tc.err == nil {
				if got != nil {
					t.Errorf("redact(nil) = %v, want nil", got)
				}
				return
			}
			if tc.password != "" && strings.Contains(got.Error(), tc.password) {
				t.Errorf("redact left the password in %q", got.Error())
			}
			if !strings.Contains(got.Error(), strings.SplitN(tc.want, " ", 2)[0]) {
				t.Errorf("redact(%v) = %q, want it to contain %q", tc.err, got.Error(), tc.want)
			}
		})
	}
}

func TestCapabilitiesWithoutAGrdpClientAreRefused(t *testing.T) {
	// These methods moved onto the concrete grdp client when the requirements
	// landed. A session built without one — which happens in tests, and would
	// happen in a future transport — must say so rather than panicking or
	// handing back an empty buffer that looks like a picture that never comes.
	s, _ := newTestSession()
	defer s.Close()

	if _, err := s.Framebuffer(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Framebuffer() = %v, want ErrUnsupported", err)
	}
	if err := s.OnFrame(nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("OnFrame() = %v, want ErrUnsupported", err)
	}
	if err := s.OnCursor(nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("OnCursor() = %v, want ErrUnsupported", err)
	}
	if err := s.OnCursorPos(nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("OnCursorPos() = %v, want ErrUnsupported", err)
	}
	if err := s.RequestResize(1280, 720); !errors.Is(err, ErrUnsupported) {
		t.Errorf("RequestResize() = %v, want ErrUnsupported", err)
	}
	if err := s.OnResize(nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("OnResize() = %v, want ErrUnsupported", err)
	}
	// The fake sink has no Unicode key event either, and says which capability
	// is missing rather than blaming a grdp version.
	if err := s.UnicodeKey('中'); !errors.Is(err, ErrUnsupported) {
		t.Errorf("UnicodeKey() = %v, want ErrUnsupported", err)
	}
}

func TestNothingStillClaimsAPendingRequirement(t *testing.T) {
	// The requirements list is a historical record; no runtime error should
	// still be telling an operator to go and change grdp.
	s, _ := newTestSession()
	defer s.Close()

	for _, err := range []error{
		func() error { _, e := s.Framebuffer(); return e }(),
		s.OnFrame(nil),
		s.OnCursor(nil),
		s.OnCursorPos(nil),
		s.RequestResize(1280, 720),
		s.OnResize(nil),
		s.UnicodeKey('中'),
		s.TypeText("hello"),
	} {
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "requirement") || errors.Is(err, ErrGRDPPending) {
			t.Errorf("error still refers to a pending requirement: %v", err)
		}
	}
}

func TestSessionStartsExactlyOnePump(t *testing.T) {
	// A dial that got far enough to fail once exercised a path where two pumps
	// were started, and both closed the events channel. The panic only appeared
	// once a dial actually connected, which is why this drives a real socket
	// that accepts and hangs up.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = Dial(ctx, Config{
		Host: listener.Addr().String(), User: "u", Password: "p",
		Width: 640, Height: 480, Timeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("Dial succeeded against a socket that hangs up")
	}
	// Reaching here at all is the assertion: a second close of the events
	// channel would have panicked the test binary.
	if CodeOf(err) == "" {
		t.Errorf("Dial returned %v with no code", err)
	}
}

func TestSessionStartIsIdempotent(t *testing.T) {
	s := &Session{
		sink:   &fakeSink{},
		done:   make(chan struct{}),
		queue:  make(chan Event, 4),
		events: make(chan Event, 4),
	}
	for i := 0; i < 5; i++ {
		s.start()
	}
	s.Close()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-s.events:
			if !ok {
				return // closed exactly once, no panic
			}
		case <-deadline:
			t.Fatal("events was never closed")
		}
	}
}

func TestDialClassifiesAnUnreachableHostWithASentinel(t *testing.T) {
	// 127.0.0.1:1 has nothing listening, so this fails immediately and without a
	// network. It is the one classification that can be checked on any machine,
	// and it checks the whole path: grdp has to attach its sentinel, and this
	// package has to keep the chain intact rather than flattening the error
	// while scrubbing a password out of it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Dial(ctx, Config{
		Host: "127.0.0.1:1", User: "u", Password: "p",
		Width: 640, Height: 480, Timeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("Dial succeeded against a closed port")
	}

	if !errors.Is(err, client.ErrUnreachable) {
		t.Errorf("the error does not carry grdp's ErrUnreachable, so the code below "+
			"came from text matching rather than from the sentinel: %v", err)
	}
	if got := CodeOf(err); got != CodeHostUnreachable {
		t.Errorf("CodeOf = %q, want %q", got, CodeHostUnreachable)
	}
}

func TestRedactIgnoresCoincidentalSubstrings(t *testing.T) {
	// A one-character password is a substring of half the alphabet. Redacting it
	// loosely mangles the message and drops the chain, which undoes the
	// sentinels for every caller above this package.
	cases := []struct {
		name     string
		msg      string
		password string
		want     string
	}{
		{"a letter inside a word", "dial tcp 127.0.0.1:1: connection refused", "p", "dial tcp 127.0.0.1:1: connection refused"},
		{"a word inside another word", "the authentication failed", "auth", "the authentication failed"},
		{"a whole word", "passwd=secret here", "secret", "passwd=[redacted] here"},
		{"a field at the end", "user: admin, passwd:secret", "secret", "user: admin, passwd:[redacted]"},
		{"at the start", "secret is the password", "secret", "[redacted] is the password"},
		{"next to punctuation", "password (secret).", "secret", "password ([redacted])."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redact(errors.New(tc.msg), tc.password).Error()
			if got != tc.want {
				t.Errorf("redact(%q, %q) = %q, want %q", tc.msg, tc.password, got, tc.want)
			}
		})
	}
}

func TestRedactKeepsTheChainWhenThereIsNothingToScrub(t *testing.T) {
	// Flattening the error here would silently undo the sentinels for every
	// caller above this package.
	sentinel := client.ErrTLSFailure
	wrapped := fmt.Errorf("sec: %w", sentinel)

	kept := redact(wrapped, "a-password-that-is-not-present")
	if !errors.Is(kept, sentinel) {
		t.Errorf("redact dropped the chain: %v", kept)
	}

	// When a credential really is in the text it goes, and the chain with it.
	const password = "s3cret"
	leaked := fmt.Errorf("login failed for %s: %w", password, sentinel)
	scrubbed := redact(leaked, password)
	if strings.Contains(scrubbed.Error(), password) {
		t.Errorf("redact left the password in %q", scrubbed.Error())
	}
	if !strings.Contains(scrubbed.Error(), "[redacted]") {
		t.Errorf("redact did not mark what it removed: %q", scrubbed.Error())
	}
}
