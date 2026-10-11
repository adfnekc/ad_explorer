// Package wsframe is the wire format between the browser and the gateway:
// a compact binary frame for pixels and JSON for everything else.
//
// Everything here is a pure function of its input so the format can be tested
// without a socket, and so a malformed frame is caught here rather than three
// layers deeper.
package wsframe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
)

// Frame types. The value sits in the first header byte.
const (
	TypeImage  uint8 = 0x01 // an encoded region of the remote desktop
	TypeCursor uint8 = 0x02 // an encoded pointer shape
	TypeFile   uint8 = 0x03 // a chunk of a transferred file (M3)
)

// Pixel formats for TypeImage / TypeCursor.
const (
	FormatWebP uint8 = 0x01
	FormatJPEG uint8 = 0x02
	FormatPNG  uint8 = 0x03 // used for cursor shapes, which need alpha
	// FormatRaw is for file chunks, whose bytes are not an image at all.
	FormatRaw uint8 = 0x04
)

// Header flags. A set bit means the corresponding statement is true.
const (
	// FlagFull marks a frame that covers the whole desktop, so the client may
	// ignore the rectangle and blit to the origin. It exists so the sender can
	// stop pretending a full repaint is a very large dirty rectangle.
	FlagFull uint16 = 1 << 0
	// FlagBatchEnd marks the last frame of one batch, which is one complete
	// redraw. The client composes the desktop off screen and presents it once,
	// and this is the signal that says the composition is whole: without it the
	// presentation happens whenever the display next refreshes, which can be
	// half way through painting the pieces of the next desktop.
	FlagBatchEnd uint16 = 1 << 2
	// FlagKeyframe marks the first frame of a stream, after which the client
	// has something to draw on.
	FlagKeyframe uint16 = 1 << 1
)

// HeaderLen is the size of the fixed header that precedes every binary frame.
const HeaderLen = 20

// MaxPayload caps a single frame. A remote desktop encoded at any sane quality
// is far below this; the limit exists so a corrupt length field cannot make the
// gateway allocate a gigabyte.
const MaxPayload = 32 << 20

var (
	// ErrShortFrame means fewer bytes arrived than the header or the declared
	// payload requires.
	ErrShortFrame = errors.New("wsframe: truncated frame")
	// ErrPayloadTooLarge means the declared payload exceeds MaxPayload.
	ErrPayloadTooLarge = errors.New("wsframe: payload too large")
	// ErrBadRect means the rectangle cannot be represented in the header.
	ErrBadRect = errors.New("wsframe: rectangle out of range")
	// ErrUnknownType means the type byte is not one this version defines.
	ErrUnknownType = errors.New("wsframe: unknown frame type")
)

// Frame is one binary message.
//
// Rect is in remote-desktop pixels and is only meaningful for TypeImage.
// For TypeCursor it carries the hotspot in Min and the shape size in Dx/Dy.
type Frame struct {
	Type    uint8
	Format  uint8
	Flags   uint16
	Rect    image.Rectangle
	Seq     uint32
	Payload []byte
}

// IsFull reports whether the frame covers the whole desktop.
func (f Frame) IsFull() bool { return f.Flags&FlagFull != 0 }

// Header encodes the fixed 20 byte header into dst, which must be at least
// HeaderLen long.
//
// Layout (all multi-byte fields little endian):
//
//	0  1  type
//	1  1  format
//	2  2  flags
//	4  2  x
//	6  2  y
//	8  2  width
//	10 2  height
//	12 4  sequence
//	16 4  payload length
func (f Frame) Header(dst []byte) error {
	if len(dst) < HeaderLen {
		return fmt.Errorf("wsframe: header buffer is %d bytes, need %d", len(dst), HeaderLen)
	}
	if err := checkType(f.Type); err != nil {
		return err
	}
	if err := checkRect(f.Rect); err != nil {
		return err
	}
	if len(f.Payload) > MaxPayload {
		return ErrPayloadTooLarge
	}

	dst[0] = f.Type
	dst[1] = f.Format
	binary.LittleEndian.PutUint16(dst[2:4], f.Flags)
	binary.LittleEndian.PutUint16(dst[4:6], uint16(f.Rect.Min.X))
	binary.LittleEndian.PutUint16(dst[6:8], uint16(f.Rect.Min.Y))
	binary.LittleEndian.PutUint16(dst[8:10], uint16(f.Rect.Dx()))
	binary.LittleEndian.PutUint16(dst[10:12], uint16(f.Rect.Dy()))
	binary.LittleEndian.PutUint32(dst[12:16], f.Seq)
	binary.LittleEndian.PutUint32(dst[16:20], uint32(len(f.Payload)))
	return nil
}

// AppendTo appends the whole frame, header and payload, to dst.
//
// It is the single allocation path used by the writer, and it never aliases
// Payload, so the caller may reuse the pixel buffer afterwards.
func (f Frame) AppendTo(dst []byte) ([]byte, error) {
	var hdr [HeaderLen]byte
	if err := f.Header(hdr[:]); err != nil {
		return dst, err
	}
	dst = append(dst, hdr[:]...)
	dst = append(dst, f.Payload...)
	return dst, nil
}

// WriteFrame writes one frame to w.
func WriteFrame(w io.Writer, f Frame) error {
	buf, err := f.AppendTo(nil)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads exactly one frame from r.
//
// Payload is freshly allocated and owned by the caller.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [HeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return Frame{}, fmt.Errorf("%w: header", ErrShortFrame)
		}
		return Frame{}, err
	}
	f, n, err := decodeHeader(hdr[:])
	if err != nil {
		return Frame{}, err
	}
	if n == 0 {
		return f, nil
	}
	f.Payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return Frame{}, fmt.Errorf("%w: payload", ErrShortFrame)
	}
	return f, nil
}

// decodeHeader validates a fixed header and reports the declared payload
// length. Both the streaming and the in-memory reader go through here, so the
// two can never disagree about what a valid header is.
func decodeHeader(hdr []byte) (Frame, int, error) {
	if len(hdr) < HeaderLen {
		return Frame{}, 0, fmt.Errorf("%w: header needs %d bytes", ErrShortFrame, HeaderLen)
	}
	// image.Rect normalises its arguments, which would move a non-zero origin
	// to (0,0); the rectangle is built field by field instead.
	f := Frame{
		Type:   hdr[0],
		Format: hdr[1],
		Flags:  binary.LittleEndian.Uint16(hdr[2:4]),
		Rect: image.Rectangle{
			Min: image.Point{
				X: int(binary.LittleEndian.Uint16(hdr[4:6])),
				Y: int(binary.LittleEndian.Uint16(hdr[6:8])),
			},
		},
		Seq: binary.LittleEndian.Uint32(hdr[12:16]),
	}
	f.Rect.Max = image.Point{
		X: f.Rect.Min.X + int(binary.LittleEndian.Uint16(hdr[8:10])),
		Y: f.Rect.Min.Y + int(binary.LittleEndian.Uint16(hdr[10:12])),
	}

	if err := checkType(f.Type); err != nil {
		return Frame{}, 0, err
	}

	n := int(binary.LittleEndian.Uint32(hdr[16:20]))
	if n > MaxPayload {
		return Frame{}, 0, fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, n)
	}
	return f, n, nil
}

// ParseFrame decodes a complete frame already in memory.
//
// Any bytes past the declared payload are ignored, so a caller reading out of a
// larger buffer does not have to slice it first.
func ParseFrame(b []byte) (Frame, error) {
	f, n, err := decodeHeader(b)
	if err != nil {
		return Frame{}, err
	}
	if len(b) < HeaderLen+n {
		return Frame{}, fmt.Errorf("%w: declared %d payload bytes, have %d", ErrShortFrame, n, len(b)-HeaderLen)
	}
	f.Payload = append([]byte(nil), b[HeaderLen:HeaderLen+n]...)
	return f, nil
}

func checkType(t uint8) error {
	switch t {
	case TypeImage, TypeCursor, TypeFile:
		return nil
	default:
		return fmt.Errorf("%w: 0x%02x", ErrUnknownType, t)
	}
}

// checkRect rejects rectangles the header cannot express. The wire format
// stores the origin and the size as separate 16 bit fields, so the constraint
// is on each of those, not on Max: an origin of 65535 with a width of 65535 is
// perfectly encodable, even though it describes a region far outside any
// desktop. Consumers clip; the codec must not silently reject or truncate a
// coordinate.
func checkRect(r image.Rectangle) error {
	if r.Min.X < 0 || r.Min.Y < 0 {
		return fmt.Errorf("%w: negative origin %v", ErrBadRect, r)
	}
	if r.Dx() < 0 || r.Dy() < 0 {
		return fmt.Errorf("%w: negative size %v", ErrBadRect, r)
	}
	for _, v := range [...]struct {
		name string
		val  int
	}{
		{"x", r.Min.X}, {"y", r.Min.Y}, {"width", r.Dx()}, {"height", r.Dy()},
	} {
		if v.val > 0xffff {
			return fmt.Errorf("%w: %s %d exceeds the 16 bit field", ErrBadRect, v.name, v.val)
		}
	}
	return nil
}

// FileOffsetLen is the prefix a file chunk carries before its bytes.
//
// A chunk states its own offset rather than relying on the order messages
// arrived in, so a retry or an out of order delivery cannot silently write bytes
// into the wrong place in a file.
const FileOffsetLen = 4

// EncodeFileChunk builds a file frame: the token in the sequence field and the
// offset in front of the payload.
//
// The token is whatever the two sides agreed on — a FilesNeed token in one
// direction, a transfer id in the other — so a chunk never has to be matched
// against a preceding control frame to be placed.
func FileChunkFrame(token, offset uint32, data []byte) (Frame, error) {
	if len(data) > MaxPayload-FileOffsetLen {
		return Frame{}, fmt.Errorf("%w: chunk of %d bytes", ErrPayloadTooLarge, len(data))
	}
	payload := make([]byte, FileOffsetLen+len(data))
	binary.LittleEndian.PutUint32(payload[0:FileOffsetLen], offset)
	copy(payload[FileOffsetLen:], data)

	return Frame{Type: TypeFile, Format: FormatRaw, Seq: token, Payload: payload}, nil
}

// EncodeFileChunk builds a file frame and returns its wire bytes.
func EncodeFileChunk(token, offset uint32, data []byte) ([]byte, error) {
	f, err := FileChunkFrame(token, offset, data)
	if err != nil {
		return nil, err
	}
	return f.AppendTo(nil)
}

// ParseFileChunk reads the offset and bytes out of a file frame.
func ParseFileChunk(f Frame) (token, offset uint32, data []byte, err error) {
	if f.Type != TypeFile {
		return 0, 0, nil, fmt.Errorf("%w: type %#x is not a file chunk", ErrUnknownType, f.Type)
	}
	if len(f.Payload) < FileOffsetLen {
		return 0, 0, nil, fmt.Errorf("%w: file chunk carries no offset", ErrShortFrame)
	}
	token = f.Seq
	offset = binary.LittleEndian.Uint32(f.Payload[0:FileOffsetLen])
	return token, offset, f.Payload[FileOffsetLen:], nil
}
