package wsframe

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"strings"
	"testing"
)

// everyClientControl lists one valid value of each type a client may send, so
// the tests iterate the real set instead of a hand-copied list.
func everyClientControl() []Control {
	return []Control{
		InputKey{Code: "KeyA", Down: true},
		InputMouse{X: 10, Y: 20, Action: PointerDown, Button: 0},
		InputWheel{X: 10, Y: 20, Delta: -3},
		InputText{Text: "中文"},
		ClipboardSet{Text: "hello"},
		ClipboardRequest{},
		SessionQuality{Level: 70},
		SessionResize{Width: 1920, Height: 1080},
		SessionClose{},
		SessionCAD{},
		SessionRepaint{},
		FilesOffer{Files: []OfferedFile{{Name: "a.iso", Size: 12}}},
		FilesClear{},
		FilesGet{Index: 2},
		FilesCancel{TransferID: 7},
		Ping{T: 1234567890},
	}
}

func TestDecodeEncodeRoundTrip(t *testing.T) {
	for _, want := range everyClientControl() {
		t.Run(want.ControlType(), func(t *testing.T) {
			b, err := EncodeControl(want)
			if err != nil {
				t.Fatalf("EncodeControl: %v", err)
			}
			got, err := DecodeControl(b)
			if err != nil {
				t.Fatalf("DecodeControl(%s): %v", b, err)
			}
			if got.ControlType() != want.ControlType() {
				t.Fatalf("type = %q, want %q", got.ControlType(), want.ControlType())
			}
			// Compare through JSON so unexported or uncomparable fields do not
			// force hand-written equality for each type.
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if !sameJSON(t, gb, wb) {
				t.Errorf("round trip changed the payload:\n got %s\nwant %s", gb, wb)
			}
		})
	}
}

func TestEncodeControlStampsType(t *testing.T) {
	// The type is injected by the encoder, so a control value that does not
	// carry one still lands on the wire correctly.
	b, err := EncodeControl(InputKey{Code: "F11"})
	if err != nil {
		t.Fatalf("EncodeControl: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "input.key" {
		t.Errorf("type = %v, want input.key", m["type"])
	}
}

func TestClientToServerTypesAreAllRegistered(t *testing.T) {
	if len(clientToServerTypes) != len(everyClientControl()) {
		t.Fatalf("clientToServerTypes has %d entries, the test covers %d — keep them in sync",
			len(clientToServerTypes), len(everyClientControl()))
	}
	for _, name := range clientToServerTypes {
		if _, ok := controlByType[name]; !ok {
			t.Errorf("controlToServerTypes lists %q but it is not registered", name)
		}
	}
}

func TestDecodeControlRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty object", `{}`, ErrMissingType},
		{"not json", `nonsense`, nil},
		{"array", `[]`, nil},
		{"server-only type", `{"type":"session.ready","width":1}`, ErrUnknownControl},
		{"made up type", `{"type":"input.teleport"}`, ErrUnknownControl},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeControl([]byte(tc.in))
			if err == nil {
				t.Fatalf("DecodeControl(%s) succeeded, want an error", tc.in)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("DecodeControl(%s) = %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

func TestDecodeControlIgnoresUnknownFields(t *testing.T) {
	// A newer client may add a field; the gateway must not reject the whole
	// message over it, or an upgrade becomes a breaking change.
	_, err := DecodeControl([]byte(`{"type":"input.key","code":"KeyA","down":true,"pressure":0.5}`))
	if err != nil {
		t.Errorf("DecodeControl rejected an unknown field: %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		in      validator
		wantErr bool
	}{
		{"key ok", InputKey{Code: "KeyA"}, false},
		{"key missing code", InputKey{}, true},

		{"mouse move", InputMouse{X: 0, Y: 0, Action: PointerMove}, false},
		{"mouse down left", InputMouse{X: 10, Y: 10, Action: PointerDown, Button: 0}, false},
		{"mouse up right", InputMouse{X: 65535, Y: 65535, Action: PointerUp, Button: 2}, false},
		{"mouse down button too high", InputMouse{Action: PointerDown, Button: 3}, true},
		{"mouse down negative button", InputMouse{Action: PointerUp, Button: -1}, true},
		{"mouse unknown action", InputMouse{Action: "wiggle"}, true},
		{"mouse empty action", InputMouse{}, true},
		{"mouse x out of range", InputMouse{X: 65536, Action: PointerMove}, true},
		{"mouse y negative", InputMouse{Y: -1, Action: PointerMove}, true},

		{"wheel ok", InputWheel{X: 1, Y: 1, Delta: 127}, false},
		{"wheel delta too large", InputWheel{Delta: MaxWheelNotches + 1}, true},
		{"wheel delta too small", InputWheel{Delta: -MaxWheelNotches - 1}, true},

		{"text ok ascii", InputText{Text: "a"}, false},
		{"text ok cjk", InputText{Text: "中文测试"}, false},
		{"text ok emoji", InputText{Text: "🙂"}, false},
		{"text empty", InputText{}, true},
		{"text too long", InputText{Text: strings.Repeat("中", MaxTextRunes+1)}, true},
		{"text not utf8", InputText{Text: "\xff\xfe"}, true},

		{"clipboard ok empty", ClipboardSet{}, false},
		{"clipboard not utf8", ClipboardSet{Text: "\xff"}, true},
		{"clipboard too long", ClipboardSet{Text: strings.Repeat("x", MaxTextRunes+1)}, true},

		{"quality low bound", SessionQuality{Level: MinQuality}, false},
		{"quality high bound", SessionQuality{Level: MaxQuality}, false},
		{"quality zero", SessionQuality{Level: 0}, true},
		{"quality too high", SessionQuality{Level: 101}, true},

		{"resize ok", SessionResize{Width: 1280, Height: 720}, false},
		{"resize too small", SessionResize{Width: 100, Height: 720}, true},
		{"resize too large", SessionResize{Width: 1280, Height: 20000}, true},

		{"offer ok", FilesOffer{Files: []OfferedFile{{Name: "a.iso", Size: 1}}}, false},
		{"offer empty", FilesOffer{}, true},
		{"offer with no name", FilesOffer{Files: []OfferedFile{{Size: 1}}}, true},
		{"offer with a negative size", FilesOffer{Files: []OfferedFile{{Name: "a", Size: -1}}}, true},
		{"offer too many", FilesOffer{Files: manyFiles(MaxOfferedFiles + 1)}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.wantErr && err == nil {
				t.Error("Validate() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if err != nil && !errors.Is(err, ErrBadField) {
				t.Errorf("Validate() = %v, want it to wrap ErrBadField", err)
			}
		})
	}
}

func TestValidateRunsOnDecode(t *testing.T) {
	// Validation must happen at the edge, not wherever the field is first used.
	bad := []string{
		`{"type":"input.key"}`,
		`{"type":"input.mouse","x":-5,"y":0,"action":"move"}`,
		`{"type":"input.text","text":""}`,
		`{"type":"session.quality","level":9999}`,
		`{"type":"session.resize","width":1,"height":1}`,
	}
	for _, in := range bad {
		if _, err := DecodeControl([]byte(in)); !errors.Is(err, ErrBadField) {
			t.Errorf("DecodeControl(%s) = %v, want ErrBadField", in, err)
		}
	}
}

func TestServerControlsEncodeWithTheirType(t *testing.T) {
	cases := []struct {
		in   Control
		want string
	}{
		{SessionReady{Width: 1280, Height: 720, Name: "web-01", Protocol: "rdp"}, "session.ready"},
		{SessionStats{FPS: 60, KBps: 512, Quality: 70, Dropped: 3}, "session.stats"},
		{ClipboardText{Text: "hi", TS: 1}, "clipboard.text"},
		{CursorPos{X: 3, Y: 4}, "cursor.pos"},
		{ErrorMsgFrom(ErrCodeNLAFailed, "credssp failed"), "error"},
		{SessionClosed{Reason: CloseIdleTimeout}, "session.closed"},
		{Pong{T: 42}, "pong"},
	}

	for _, tc := range cases {
		b, err := EncodeControl(tc.in)
		if err != nil {
			t.Fatalf("EncodeControl(%T): %v", tc.in, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m["type"] != tc.want {
			t.Errorf("%T type = %v, want %q", tc.in, m["type"], tc.want)
		}
	}
}

func TestErrorMsgFrom(t *testing.T) {
	e := ErrorMsgFrom(ErrCodeHostUnreach, "dial tcp: connection refused")
	if e.Code != ErrCodeHostUnreach || e.Message == "" {
		t.Errorf("ErrorMsgFrom = %+v", e)
	}
}

// manyFiles builds n distinct offered files.
func manyFiles(n int) []OfferedFile {
	out := make([]OfferedFile, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, OfferedFile{Name: fmt.Sprintf("f%d", i), Size: 1})
	}
	return out
}

func TestFileChunkRoundTrip(t *testing.T) {
	// A chunk states its own offset, so it can be placed without a preceding
	// control frame and an out of order delivery cannot write to the wrong
	// place in a file.
	payload := []byte("these are the bytes of a file")
	raw, err := EncodeFileChunk(0xdeadbeef, 4096, payload)
	if err != nil {
		t.Fatalf("EncodeFileChunk: %v", err)
	}

	frame, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}
	token, offset, data, err := ParseFileChunk(frame)
	if err != nil {
		t.Fatalf("ParseFileChunk: %v", err)
	}
	if token != 0xdeadbeef {
		t.Errorf("token = %#x, want 0xdeadbeef", token)
	}
	if offset != 4096 {
		t.Errorf("offset = %d, want 4096", offset)
	}
	if string(data) != string(payload) {
		t.Errorf("data = %q, want %q", data, payload)
	}
}

func TestFileChunkRejectsTheWrongFrame(t *testing.T) {
	imageFrame := Frame{Type: TypeImage, Format: FormatJPEG, Rect: image.Rect(0, 0, 1, 1), Payload: []byte{1, 2, 3, 4}}
	if _, _, _, err := ParseFileChunk(imageFrame); err == nil {
		t.Error("ParseFileChunk accepted an image frame")
	}

	short := Frame{Type: TypeFile, Format: FormatRaw, Payload: []byte{1, 2}}
	if _, _, _, err := ParseFileChunk(short); !errors.Is(err, ErrShortFrame) {
		t.Errorf("ParseFileChunk(short) = %v, want ErrShortFrame", err)
	}
}

func TestFileChunkRejectsAnOversizedChunk(t *testing.T) {
	if _, err := EncodeFileChunk(1, 0, make([]byte, MaxPayload)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("EncodeFileChunk(huge) = %v, want ErrPayloadTooLarge", err)
	}
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv map[string]any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if len(av) != len(bv) {
		return false
	}
	for k, v := range av {
		w, ok := bv[k]
		if !ok {
			return false
		}
		// Numbers arrive as float64 on both sides, so a plain compare is fine.
		if toJSON(v) != toJSON(w) {
			return false
		}
	}
	return true
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
