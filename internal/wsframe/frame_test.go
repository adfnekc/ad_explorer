package wsframe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Frame
	}{
		{
			name: "small dirty rect",
			in: Frame{
				Type: TypeImage, Format: FormatJPEG, Seq: 7,
				Rect:    image.Rect(16, 32, 144, 96),
				Payload: []byte{0xde, 0xad, 0xbe, 0xef},
			},
		},
		{
			name: "full frame flag",
			in: Frame{
				Type: TypeImage, Format: FormatWebP, Flags: FlagFull | FlagKeyframe, Seq: 1,
				Rect:    image.Rect(0, 0, 1920, 1080),
				Payload: bytes.Repeat([]byte{0x42}, 1000),
			},
		},
		{
			name: "cursor with hotspot and no payload",
			in: Frame{
				Type: TypeCursor, Format: FormatPNG, Seq: 2,
				Rect: image.Rect(4, 7, 4+24, 7+24),
			},
		},
		{
			name: "one byte payload",
			in: Frame{
				Type: TypeFile, Format: FormatRaw, Seq: 0,
				Rect:    image.Rect(0, 0, 1, 1),
				Payload: []byte{0xff},
			},
		},
		{
			name: "max coordinates",
			in: Frame{
				Type: TypeImage, Format: FormatWebP,
				Rect:    image.Rect(0xffff-1, 0xffff-1, 0xffff, 0xffff),
				Payload: nil,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, err := tc.in.AppendTo(nil)
			if err != nil {
				t.Fatalf("AppendTo: %v", err)
			}
			if len(buf) != HeaderLen+len(tc.in.Payload) {
				t.Fatalf("encoded %d bytes, want %d", len(buf), HeaderLen+len(tc.in.Payload))
			}

			got, err := ParseFrame(buf)
			if err != nil {
				t.Fatalf("ParseFrame: %v", err)
			}
			if got.Type != tc.in.Type || got.Format != tc.in.Format || got.Flags != tc.in.Flags {
				t.Errorf("header mismatch: got type=%#x format=%#x flags=%#x",
					got.Type, got.Format, got.Flags)
			}
			if got.Rect != tc.in.Rect {
				t.Errorf("rect = %v, want %v", got.Rect, tc.in.Rect)
			}
			if got.Seq != tc.in.Seq {
				t.Errorf("seq = %d, want %d", got.Seq, tc.in.Seq)
			}
			if !bytes.Equal(got.Payload, tc.in.Payload) {
				t.Errorf("payload = %x, want %x", got.Payload, tc.in.Payload)
			}
		})
	}
}

func TestFrameHeaderLayoutIsStable(t *testing.T) {
	// The header is a contract with the browser; pin every byte offset.
	f := Frame{
		Type: TypeImage, Format: FormatWebP, Flags: FlagFull, Seq: 0x11223344,
		Rect:    image.Rect(0x0102, 0x0304, 0x0102+0x0506, 0x0304+0x0708),
		Payload: []byte{0xaa},
	}
	buf, err := f.AppendTo(nil)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}

	want := []byte{
		0x01,       // type
		0x01,       // format
		0x01, 0x00, // flags = FlagFull
		0x02, 0x01, // x = 0x0102
		0x04, 0x03, // y = 0x0304
		0x06, 0x05, // width = 0x0506
		0x08, 0x07, // height = 0x0708
		0x44, 0x33, 0x22, 0x11, // seq
		0x01, 0x00, 0x00, 0x00, // payload length
		0xaa,
	}
	if !bytes.Equal(buf, want) {
		t.Errorf("header bytes:\n got %x\nwant %x", buf, want)
	}
}

func TestAppendToDoesNotAliasPayload(t *testing.T) {
	payload := []byte{1, 2, 3, 4}
	f := Frame{Type: TypeImage, Format: FormatJPEG, Rect: image.Rect(0, 0, 4, 1), Payload: payload}

	buf, err := f.AppendTo(nil)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	// The encoder reuses its pixel buffer between frames; the wire bytes must
	// not move when it does.
	payload[0] = 0xff
	if buf[HeaderLen] != 1 {
		t.Errorf("wire bytes follow the caller's buffer: got %#x, want 1", buf[HeaderLen])
	}
}

func TestAppendToAppends(t *testing.T) {
	f := Frame{Type: TypeImage, Format: FormatJPEG, Rect: image.Rect(0, 0, 1, 1), Payload: []byte{9}}

	dst := []byte("prefix")
	got, err := f.AppendTo(dst)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	if string(got[:6]) != "prefix" {
		t.Errorf("AppendTo did not append: %q", got[:6])
	}
	if len(got) != 6+HeaderLen+1 {
		t.Errorf("len = %d, want %d", len(got), 6+HeaderLen+1)
	}
}

func TestReadFrameReadsConsecutiveFrames(t *testing.T) {
	var stream bytes.Buffer
	want := []Frame{
		{Type: TypeImage, Format: FormatJPEG, Seq: 1, Rect: image.Rect(0, 0, 8, 8), Payload: []byte("first")},
		{Type: TypeCursor, Format: FormatPNG, Seq: 2, Rect: image.Rect(1, 2, 25, 26), Payload: []byte("second")},
		{Type: TypeImage, Format: FormatWebP, Seq: 3, Rect: image.Rect(0, 0, 4, 4)},
	}
	for _, f := range want {
		if err := WriteFrame(&stream, f); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}

	for i, w := range want {
		got, err := ReadFrame(&stream)
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if got.Seq != w.Seq || got.Type != w.Type || got.Rect != w.Rect || !bytes.Equal(got.Payload, w.Payload) {
			t.Errorf("frame %d = %+v, want %+v", i, got, w)
		}
	}
	if stream.Len() != 0 {
		t.Errorf("%d bytes left over after the last frame", stream.Len())
	}
}

func TestReadFrameTruncated(t *testing.T) {
	full, err := Frame{
		Type: TypeImage, Format: FormatJPEG,
		Rect: image.Rect(0, 0, 8, 8), Payload: bytes.Repeat([]byte{1}, 50),
	}.AppendTo(nil)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}

	cases := []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"partial header", full[:HeaderLen-1]},
		{"header only", full[:HeaderLen]},
		{"partial payload", full[:HeaderLen+49]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tc.buf))
			if !errors.Is(err, ErrShortFrame) {
				t.Errorf("ReadFrame = %v, want ErrShortFrame", err)
			}
		})
	}
}

func TestAppendToAndParseFrameRejectBadInput(t *testing.T) {
	valid := Frame{Type: TypeImage, Format: FormatJPEG, Rect: image.Rect(0, 0, 4, 4)}

	t.Run("no type", func(t *testing.T) {
		f := valid
		f.Type = 0
		if _, err := f.AppendTo(nil); !errors.Is(err, ErrUnknownType) {
			t.Errorf("AppendTo = %v, want ErrUnknownType", err)
		}
	})

	t.Run("unknown type", func(t *testing.T) {
		f := valid
		f.Type = 0x7f
		if _, err := f.AppendTo(nil); !errors.Is(err, ErrUnknownType) {
			t.Errorf("AppendTo = %v, want ErrUnknownType", err)
		}
	})

	t.Run("negative rect", func(t *testing.T) {
		f := valid
		f.Rect = image.Rect(-1, 0, 4, 4)
		if _, err := f.AppendTo(nil); !errors.Is(err, ErrBadRect) {
			t.Errorf("AppendTo = %v, want ErrBadRect", err)
		}
	})

	t.Run("rect beyond 16 bits", func(t *testing.T) {
		f := valid
		f.Rect = image.Rect(0, 0, 0x10000, 4)
		if _, err := f.AppendTo(nil); !errors.Is(err, ErrBadRect) {
			t.Errorf("AppendTo = %v, want ErrBadRect", err)
		}
	})

	t.Run("payload too large", func(t *testing.T) {
		f := valid
		f.Payload = make([]byte, MaxPayload+1)
		if _, err := f.AppendTo(nil); !errors.Is(err, ErrPayloadTooLarge) {
			t.Errorf("AppendTo = %v, want ErrPayloadTooLarge", err)
		}
	})

	t.Run("header buffer too small", func(t *testing.T) {
		if err := valid.Header(make([]byte, HeaderLen-1)); err == nil {
			t.Error("Header accepted a short buffer")
		}
	})
}

func TestParseFrameRejectsHostileLength(t *testing.T) {
	// A declared length the sender never sent must be refused, not allocated.
	buf := make([]byte, HeaderLen)
	buf[0] = TypeImage
	buf[1] = FormatJPEG
	binary.LittleEndian.PutUint32(buf[16:20], MaxPayload+1)

	if _, err := ParseFrame(buf); !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("ParseFrame = %v, want ErrPayloadTooLarge", err)
	}

	binary.LittleEndian.PutUint32(buf[16:20], 1024)
	if _, err := ParseFrame(buf); !errors.Is(err, ErrShortFrame) {
		t.Errorf("ParseFrame = %v, want ErrShortFrame", err)
	}
}

func TestParseFrameRejectsUnknownType(t *testing.T) {
	buf := make([]byte, HeaderLen)
	buf[0] = 0x7f
	if _, err := ParseFrame(buf); !errors.Is(err, ErrUnknownType) {
		t.Errorf("ParseFrame = %v, want ErrUnknownType", err)
	}
}

func TestFrameAllowsOriginPlusSizeBeyondCoordinateSpace(t *testing.T) {
	// The header stores origin and size as separate 16 bit fields, so a region
	// can describe an area past the desktop. That is encodable and must survive
	// a round trip rather than being rejected: the fuzzer found this first.
	f := Frame{
		Type: TypeImage, Format: FormatJPEG,
		Rect:    image.Rect(0xf000, 0xf000, 0xf000+0xf000, 0xf000+0x8000),
		Payload: []byte{1},
	}
	buf, err := f.AppendTo(nil)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	got, err := ParseFrame(buf)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}
	if got.Rect != f.Rect {
		t.Errorf("rect = %v, want %v", got.Rect, f.Rect)
	}
}

func TestFrameIsFull(t *testing.T) {
	if !(Frame{Flags: FlagFull}).IsFull() {
		t.Error("IsFull = false with FlagFull set")
	}
	if (Frame{Flags: FlagKeyframe}).IsFull() {
		t.Error("IsFull = true with only FlagKeyframe set")
	}
}

// FuzzParseFrame checks that no input can make the decoder panic or allocate
// without bound. The seed corpus includes the shapes the tests above use.
func FuzzParseFrame(f *testing.F) {
	seeds := [][]byte{
		{},
		bytes.Repeat([]byte{0}, HeaderLen),
		func() []byte {
			b, _ := Frame{
				Type: TypeImage, Format: FormatJPEG, Flags: FlagFull,
				Rect: image.Rect(1, 2, 3, 4), Seq: 5, Payload: []byte("payload"),
			}.AppendTo(nil)
			return b
		}(),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := ParseFrame(data)
		if err != nil {
			return
		}
		// Anything that parsed must re-encode and parse back to the same frame.
		// The bytes are not compared directly because input may carry trailing
		// bytes beyond the declared payload, which ParseFrame ignores.
		again, err := got.AppendTo(nil)
		if err != nil {
			t.Fatalf("parsed frame does not re-encode: %v", err)
		}
		re, err := ParseFrame(again)
		if err != nil {
			t.Fatalf("re-encoded frame does not parse: %v", err)
		}
		if re.Type != got.Type || re.Format != got.Format || re.Flags != got.Flags ||
			re.Rect != got.Rect || re.Seq != got.Seq || !bytes.Equal(re.Payload, got.Payload) {
			t.Fatalf("round trip changed the frame:\n got %+v\nwant %+v", re, got)
		}
	})
}
