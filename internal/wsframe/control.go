package wsframe

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Control is a JSON message. Text frames carry control, binary frames carry
// pixels, so the two never have to be told apart by inspecting contents.
type Control interface {
	// ControlType is the value of the "type" field on the wire.
	ControlType() string
}

// Limits on what a peer may ask for. Generous enough for real use, tight
// enough that a malformed or hostile control frame cannot cost more than a
// bounded amount of work.
const (
	MaxCoord        = 0xffff // the RDP coordinate space is 16 bit
	MaxWheelNotches = 1024
	MaxTextRunes    = 4096
	MinDesktop      = 320
	MaxDesktop      = 7680
	MaxQuality      = 100
	MinQuality      = 1
)

// Error codes. The client maps these to messages; a code it does not know is
// shown with its raw text so a newer gateway is still diagnosable.
const (
	ErrCodeAuthRequired  = "auth_required"
	ErrCodeLoginFailed   = "login_failed"
	ErrCodeLoginLocked   = "login_locked"
	ErrCodeHostUnreach   = "host_unreachable"
	ErrCodeTLSError      = "tls_error"
	ErrCodeNLAFailed     = "nla_failed"
	ErrCodeRDPAuthFailed = "rdp_auth_failed"
	ErrCodeProtocol      = "protocol_error"
	ErrCodeSessionLimit  = "session_limit"
	ErrCodeSessionGone   = "session_gone"
	ErrCodeInternal      = "internal_error"
	// ErrCodeUnsupported means the gateway understood the request and cannot
	// honour it. It is distinct from a protocol error: the client should stop
	// asking rather than retry, and the UI should say what is missing.
	ErrCodeUnsupported = "unsupported"
	// ErrCodeInvalidRequest means the gateway rejected the request itself.
	ErrCodeInvalidRequest = "invalid_request"
	// ErrCodeBadOrigin means a state-changing request came from another site.
	ErrCodeBadOrigin = "bad_origin"
)

// Close reasons attached to SessionClosed.
const (
	CloseUserRequested  = "user_requested"
	CloseIdleTimeout    = "idle_timeout"
	CloseServerShutdown = "server_shutdown"
	CloseRemoteEnded    = "remote_ended"
	CloseReplaced       = "replaced"
	CloseError          = "error"
)

// Validation errors.
var (
	ErrMissingType    = errors.New("wsframe: control has no type")
	ErrUnknownControl = errors.New("wsframe: unknown control type")
	ErrBadField       = errors.New("wsframe: invalid field")
)

// ---------------------------------------------------------------- client → server

// InputKey is a keyboard event identified by its physical key, using the
// browser's KeyboardEvent.code, so the mapping does not depend on the client's
// layout. Characters an IME produced do not come through here; they use
// InputText, because a scancode cannot express a composed character.
type InputKey struct {
	Code      string `json:"code"`
	Down      bool   `json:"down"`
	Modifiers uint8  `json:"modifiers,omitempty"`
}

func (InputKey) ControlType() string { return "input.key" }

func (c InputKey) Validate() error {
	if c.Code == "" {
		return fmt.Errorf("%w: input.key needs code", ErrBadField)
	}
	return nil
}

// Pointer actions.
const (
	PointerMove  = "move"
	PointerDown  = "down"
	PointerUp    = "up"
	PointerWheel = "wheel"
)

// InputMouse is a pointer event. X and Y are in remote desktop pixels; the
// browser converts from its own canvas coordinates.
//
// Action says what happened. It is explicit rather than inferred from a button
// and a boolean, because "no button, not pressed" describes both a move and the
// release of the left button — a drag would arrive as a string of moves.
//
// Button follows grdp's convention, which is the sender's, not the protocol's:
// 0 is left, 1 is middle, 2 is right. It is only meaningful for down and up.
type InputMouse struct {
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Action string `json:"action"`
	Button int    `json:"button,omitempty"`
}

func (InputMouse) ControlType() string { return "input.mouse" }

func (c InputMouse) Validate() error {
	if c.X < 0 || c.X > MaxCoord || c.Y < 0 || c.Y > MaxCoord {
		return fmt.Errorf("%w: input.mouse at (%d,%d)", ErrBadField, c.X, c.Y)
	}
	switch c.Action {
	case PointerMove:
		return nil
	case PointerDown, PointerUp:
		if c.Button < 0 || c.Button > 2 {
			return fmt.Errorf("%w: input.mouse button %d", ErrBadField, c.Button)
		}
		return nil
	default:
		return fmt.Errorf("%w: input.mouse action %q", ErrBadField, c.Action)
	}
}

// InputWheel is a wheel event. Delta is in notches and positive means away
// from the user, matching grdp's MouseWheel.
type InputWheel struct {
	X     int `json:"x"`
	Y     int `json:"y"`
	Delta int `json:"delta"`
}

func (InputWheel) ControlType() string { return "input.wheel" }

func (c InputWheel) Validate() error {
	if c.X < 0 || c.X > MaxCoord || c.Y < 0 || c.Y > MaxCoord {
		return fmt.Errorf("%w: input.wheel at (%d,%d)", ErrBadField, c.X, c.Y)
	}
	if c.Delta < -MaxWheelNotches || c.Delta > MaxWheelNotches {
		return fmt.Errorf("%w: input.wheel delta %d", ErrBadField, c.Delta)
	}
	return nil
}

// InputText carries characters that must reach the remote as Unicode keyboard
// events: the result of an IME composition, and printable characters typed
// where a scancode would be wrong.
//
// Only the final result belongs here. Intermediate composition state is local
// UI and must not be sent, or the remote receives garbage.
type InputText struct {
	Text string `json:"text"`
}

func (InputText) ControlType() string { return "input.text" }

func (c InputText) Validate() error {
	if c.Text == "" {
		return fmt.Errorf("%w: input.text is empty", ErrBadField)
	}
	if !utf8.ValidString(c.Text) {
		return fmt.Errorf("%w: input.text is not valid UTF-8", ErrBadField)
	}
	if utf8.RuneCountInString(c.Text) > MaxTextRunes {
		return fmt.Errorf("%w: input.text has more than %d characters", ErrBadField, MaxTextRunes)
	}
	return nil
}

// ClipboardSet publishes local clipboard text to the remote.
type ClipboardSet struct {
	Text string `json:"text"`
}

func (ClipboardSet) ControlType() string { return "clipboard.set" }

func (c ClipboardSet) Validate() error {
	if !utf8.ValidString(c.Text) {
		return fmt.Errorf("%w: clipboard.set is not valid UTF-8", ErrBadField)
	}
	if utf8.RuneCountInString(c.Text) > MaxTextRunes {
		return fmt.Errorf("%w: clipboard.set has more than %d characters", ErrBadField, MaxTextRunes)
	}
	return nil
}

// ClipboardRequest asks the remote for the text it last announced.
//
// Some servers announce a format before they can serve it, in which case the
// automatic fetch fails and an explicit request later is what gets the data.
type ClipboardRequest struct{}

func (ClipboardRequest) ControlType() string { return "clipboard.request" }

// SessionQuality sets the target encoder quality, 1..100.
type SessionQuality struct {
	Level int `json:"level"`
}

func (SessionQuality) ControlType() string { return "session.quality" }

func (c SessionQuality) Validate() error {
	if c.Level < MinQuality || c.Level > MaxQuality {
		return fmt.Errorf("%w: session.quality %d outside %d..%d", ErrBadField, c.Level, MinQuality, MaxQuality)
	}
	return nil
}

// SessionResize asks the remote desktop to change size. It has no effect until
// the gateway can speak Display Control (grdp requirement E); the gateway
// answers with an error rather than pretending.
type SessionResize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (SessionResize) ControlType() string { return "session.resize" }

func (c SessionResize) Validate() error {
	if c.Width < MinDesktop || c.Width > MaxDesktop || c.Height < MinDesktop || c.Height > MaxDesktop {
		return fmt.Errorf("%w: session.resize %dx%d outside %d..%d", ErrBadField, c.Width, c.Height, MinDesktop, MaxDesktop)
	}
	return nil
}

// SessionClose ends the session at the client's request.
type SessionClose struct{}

func (SessionClose) ControlType() string { return "session.close" }

// SessionCAD sends Ctrl+Alt+End to the remote.
//
// It exists because Ctrl+Alt+Del cannot come from a page: the operating system
// claims that combination before the browser sees it. Ctrl+Alt+End is the
// remote session's equivalent, and it is the only way to reach a locked
// Windows host.
type SessionCAD struct{}

func (SessionCAD) ControlType() string { return "session.cad" }

// Ping carries an opaque timestamp the gateway echoes in Pong. The client uses
// it to measure round trip time for the HUD.
type Ping struct {
	T int64 `json:"t"`
}

func (Ping) ControlType() string { return "ping" }

// ---------------------------------------------------------------- server → client

// SessionReady is sent before the first frame so the client can size its canvas.
type SessionReady struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

func (SessionReady) ControlType() string { return "session.ready" }

// SessionStats drives the HUD. Sent about once a second.
type SessionStats struct {
	FPS     float64 `json:"fps"`
	KBps    float64 `json:"kbps"`
	Quality int     `json:"quality"`
	Dropped int     `json:"dropped"`
}

func (SessionStats) ControlType() string { return "session.stats" }

// ClipboardText is text the remote put on the shared clipboard. The client
// appends it to the clipboard log rather than writing it to the local
// clipboard, so a busy remote cannot overwrite what the operator is doing.
type ClipboardText struct {
	Text string `json:"text"`
	TS   int64  `json:"ts"`
}

func (ClipboardText) ControlType() string { return "clipboard.text" }

// CursorSystem tells the client to substitute its own pointer.
//
// A server can name one of its built in cursors instead of sending a shape, and
// it can ask for no pointer at all. Neither carries pixels, so neither uses the
// binary cursor frame: a pointer that keeps showing the previous shape because
// the message was undrawable is worse than one that falls back.
type CursorSystem struct {
	// SystemType is the server's raw system pointer type.
	SystemType uint32 `json:"systemType"`
	// Hidden means the server asked for no pointer.
	Hidden bool `json:"hidden"`
}

func (CursorSystem) ControlType() string { return "cursor.system" }

// CursorPos reports the remote pointer position in desktop pixels.
type CursorPos struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func (CursorPos) ControlType() string { return "cursor.pos" }

// ErrorMsg is a fatal or noteworthy condition. Code is one of the ErrCode
// constants; Message never contains a credential.
type ErrorMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (ErrorMsg) ControlType() string { return "error" }

// SessionClosed ends the client's session. Reason is one of the Close
// constants.
type SessionClosed struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

func (SessionClosed) ControlType() string { return "session.closed" }

// Pong echoes a Ping's timestamp.
type Pong struct {
	T int64 `json:"t"`
}

func (Pong) ControlType() string { return "pong" }

// ---------------------------------------------------------------- codec

type envelope struct {
	Type string `json:"type"`
}

// controlByType maps a wire type to a fresh pointer to its concrete type.
//
// Only client → server types are registered: the gateway decodes those, and
// anything else arriving from a client is a bug or an attack, so it must fail
// here rather than be quietly accepted.
var controlByType = map[string]func() Control{
	"input.key":         func() Control { return &InputKey{} },
	"input.mouse":       func() Control { return &InputMouse{} },
	"input.wheel":       func() Control { return &InputWheel{} },
	"input.text":        func() Control { return &InputText{} },
	"clipboard.set":     func() Control { return &ClipboardSet{} },
	"clipboard.request": func() Control { return &ClipboardRequest{} },
	"session.quality":   func() Control { return &SessionQuality{} },
	"session.resize":    func() Control { return &SessionResize{} },
	"session.close":     func() Control { return &SessionClose{} },
	"session.cad":       func() Control { return &SessionCAD{} },
	"files.offer":       func() Control { return &FilesOffer{} },
	"files.clear":       func() Control { return &FilesClear{} },
	"files.get":         func() Control { return &FilesGet{} },
	"files.cancel":      func() Control { return &FilesCancel{} },
	"ping":              func() Control { return &Ping{} },
}

// clientToServerTypes lists the registered types so tests can iterate them
// instead of repeating the list.
var clientToServerTypes = []string{
	"input.key", "input.mouse", "input.wheel", "input.text",
	"clipboard.set", "clipboard.request",
	"session.quality", "session.resize", "session.close", "session.cad",
	"files.offer", "files.clear", "files.get", "files.cancel",
	"ping",
}

// validator is implemented by controls that can check themselves. Controls
// with no fields to get wrong do not implement it.
type validator interface{ Validate() error }

// DecodeControl parses a control frame received from a client.
//
// The returned value is always a pointer to one of the concrete types above,
// because the decoder needs an address to unmarshal into. A type switch on a
// control therefore has to match *InputKey, not InputKey: matching the value
// type compiles cleanly and then silently handles nothing.
//
// Unknown fields are ignored so a newer client can add one without the gateway
// rejecting the whole message, but an unknown type is an error: silently
// dropping input the operator typed is worse than complaining.
func DecodeControl(b []byte) (Control, error) {
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("wsframe: control json: %w", err)
	}
	if e.Type == "" {
		return nil, ErrMissingType
	}
	mk, ok := controlByType[e.Type]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownControl, e.Type)
	}
	c := mk()
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("wsframe: control %s: %w", e.Type, err)
	}
	if v, ok := c.(validator); ok {
		if err := v.Validate(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// EncodeControl serialises a control frame, stamping the type field from
// ControlType.
//
// The stamp is applied here rather than by each caller so a new control cannot
// ship with a missing or stale type — the one mistake that would make a frame
// undecodable at the far end.
func EncodeControl(c Control) ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("wsframe: encode %T: %w", c, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("wsframe: encode %T: %w", c, err)
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage, 1)
	}
	typeJSON, err := json.Marshal(c.ControlType())
	if err != nil {
		return nil, fmt.Errorf("wsframe: encode %T: %w", c, err)
	}
	fields["type"] = typeJSON
	return json.Marshal(fields)
}

// ErrorMsgFrom builds an ErrorMsg, so call sites do not each invent a message.
func ErrorMsgFrom(code, message string) ErrorMsg {
	return ErrorMsg{Code: code, Message: message}
}

// ---------------------------------------------------------------- file transfer

// The file transfer frames carry no bytes: the payloads travel as binary frames
// of type TypeFile, because base64 would cost a third more for no benefit. One
// control frame announces what is about to be sent, and the chunks follow.

// OfferedFile is a file the client is willing to share with the remote, so that
// pasting on the remote pastes it.
type OfferedFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// FilesOffer publishes files the remote may paste.
type FilesOffer struct {
	Files []OfferedFile `json:"files"`
}

func (FilesOffer) ControlType() string { return "files.offer" }

func (c FilesOffer) Validate() error {
	if len(c.Files) == 0 {
		return fmt.Errorf("%w: files.offer is empty", ErrBadField)
	}
	if len(c.Files) > MaxOfferedFiles {
		return fmt.Errorf("%w: files.offer carries %d files, the limit is %d",
			ErrBadField, len(c.Files), MaxOfferedFiles)
	}
	for _, f := range c.Files {
		if strings.TrimSpace(f.Name) == "" {
			return fmt.Errorf("%w: an offered file has no name", ErrBadField)
		}
		if f.Size < 0 {
			return fmt.Errorf("%w: %q has a negative size", ErrBadField, f.Name)
		}
	}
	return nil
}

// FilesClear withdraws the offered files, so the remote stops being able to
// paste them.
type FilesClear struct{}

func (FilesClear) ControlType() string { return "files.clear" }

// FilesGet asks for one of the files the remote offered.
type FilesGet struct {
	Index uint32 `json:"index"`
}

func (FilesGet) ControlType() string { return "files.get" }

// FilesCancel abandons a download in progress.
type FilesCancel struct {
	TransferID uint32 `json:"transferId"`
}

func (FilesCancel) ControlType() string { return "files.cancel" }

// RemoteFile is one entry the remote put on the clipboard.
type RemoteFile struct {
	Index uint32 `json:"index"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"isDir"`
}

// FilesRemote tells the client what the remote has offered. It replaces any
// previous list, including with an empty one: the remote clearing its clipboard
// is a change the client has to hear about, or it keeps offering files that are
// no longer there.
type FilesRemote struct {
	Files []RemoteFile `json:"files"`
}

func (FilesRemote) ControlType() string { return "files.remote" }

// FilesNeed asks the client for a range of one of the files it offered. The
// gateway does not store the bytes: it asks for each range as the remote
// requests it, which is what keeps a 4 GiB file from having to fit in memory.
type FilesNeed struct {
	// Token identifies the answer. It is echoed in the sequence field of the
	// binary chunk that replies.
	Token  uint32 `json:"token"`
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

func (FilesNeed) ControlType() string { return "files.need" }

// FilesBegin announces a download. The chunks that follow carry TransferID in
// their sequence field.
type FilesBegin struct {
	TransferID uint32 `json:"transferId"`
	Index      uint32 `json:"index"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
}

func (FilesBegin) ControlType() string { return "files.begin" }

// FilesEnd closes a download, whether it finished or failed.
type FilesEnd struct {
	TransferID uint32 `json:"transferId"`
	Error      string `json:"error,omitempty"`
}

func (FilesEnd) ControlType() string { return "files.end" }

// MaxOfferedFiles bounds how many files one offer may carry. It matches what the
// clipboard's descriptor list is comfortable with rather than a protocol limit.
const MaxOfferedFiles = 256
