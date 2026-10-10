package wsframe

import (
	"io"
	"testing"
)

// The file transfer path copies each chunk several times: the frame builder
// copies the bytes behind an offset prefix, and the encoder copies the whole
// frame again on its way to the wire. Both are here so the cost per byte is a
// number rather than an opinion, which is what decides whether removing one of
// them is worth anything.
func BenchmarkFileChunk(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		data := make([]byte, size)
		b.Run(map[int]string{64 << 10: "64KiB", 1 << 20: "1MiB"}[size], func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := EncodeFileChunk(1, 0, data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ParseFileChunk should not copy: it reads a prefix off the front and hands back
// the rest.
func BenchmarkParseFileChunk(b *testing.B) {
	const size = 1 << 20
	// The gateway parses the frame off the wire first, so this is handed a
	// frame rather than bytes. That is the point: the payload is a slice of what
	// was read, and no copy happens on the way in.
	frame, err := FileChunkFrame(1, 0, make([]byte, size))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, offset, data, err := ParseFileChunk(frame)
		if err != nil {
			b.Fatal(err)
		}
		if len(data) != size || offset != 0 {
			b.Fatalf("offset %d, %d bytes", offset, len(data))
		}
		io.Discard.Write(data)
	}
}
