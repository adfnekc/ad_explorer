// Command adx-mockframes serves the real frontend and feeds it a scripted
// sequence of frames, so the drawing can be checked without a remote desktop and
// without a real session behind it.
//
// It exists because the failure it is for is intermittent: a desktop that is
// painted wrongly and stays that way is either a frame that was never drawn or a
// frame drawn in the wrong order, and neither can be reproduced on demand
// against a live target. Here the frames and their order are the whole input, so
// a wrong canvas is a wrong client.
//
// The script is deliberately adversarial:
//
//	1  a full frame painting a grid of greys
//	2  a region repainting cell (0,0) red
//	3  a region repainting the same cell blue, sent with no delay after it
//	4  a full frame resetting the grid
//	5  a region repainting cell (0,0) green
//	6  a region repainting cell (1,1) yellow
//
// All of them go out in one burst, so the client's queue is holding frames while
// it decodes. A correct client ends with cell (0,0) green, cell (1,1) yellow and
// the rest of the grid grey. Anything else is the bug.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/adfnekc/ad_explorer/internal/webui"
	"github.com/gorilla/websocket"
)

const (
	headerLen  = 20
	typeImage  = 0x01
	formatJPEG = 0x02
	flagFull   = 1 << 0
)

var grid = [3][4]color.RGBA{
	{{200, 200, 200, 255}, {180, 180, 180, 255}, {160, 160, 160, 255}, {140, 140, 140, 255}},
	{{120, 120, 120, 255}, {100, 100, 100, 255}, {80, 80, 80, 255}, {60, 60, 60, 255}},
	{{220, 220, 220, 255}, {210, 210, 210, 255}, {190, 190, 190, 255}, {170, 170, 170, 255}},
}

const (
	width  = 320
	height = 240
	cellW  = width / 4
	cellH  = height / 3
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18650", "address to serve on")
	flag.Parse()

	assets, err := webui.Assets()
	if err != nil {
		log.Fatal(err)
	}
	files := http.FileServerFS(assets)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, []any{})
			return
		}
		// The frontend reads these fields and nothing else is required, so the
		// answer is the shape rather than a session.
		writeJSON(w, map[string]any{
			"id": "mock", "name": "mock-frames", "host": "mock:0",
			"width": width, "height": height, "reconnectGraceMs": 0,
		})
	})
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			serveStream(w, r)
			return
		}
		writeJSON(w, map[string]any{})
	})
	mux.Handle("/", files)

	log.Printf("mock frames on http://%s/", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func serveStream(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	defer conn.Close()

	// The client needs the size before it can place anything, and it reads this
	// as a control message rather than a frame.
	ready, _ := json.Marshal(map[string]any{
		"type": "session.ready", "id": "mock", "width": width, "height": height,
	})
	if err := conn.WriteMessage(websocket.TextMessage, ready); err != nil {
		return
	}

	// The whole script goes out with no pauses between frames, which is what
	// fills the client's queue and makes it decode several at once.
	seq := uint32(0)
	send := func(full bool, x, y, w, h int, img image.Image) {
		seq++
		b, err := frame(seq, full, x, y, w, h, img)
		if err != nil {
			log.Printf("frame %d: %v", seq, err)
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
			log.Printf("write %d: %v", seq, err)
		}
	}

	send(true, 0, 0, width, height, gridImage())
	send(false, 0, 0, cellW, cellH, solid(color.RGBA{220, 40, 40, 255}, cellW, cellH))
	send(false, 0, 0, cellW, cellH, solid(color.RGBA{40, 40, 220, 255}, cellW, cellH))
	send(true, 0, 0, width, height, gridImage())
	send(false, 0, 0, cellW, cellH, solid(color.RGBA{40, 200, 40, 255}, cellW, cellH))
	send(false, cellW, cellH, cellW, cellH, solid(color.RGBA{230, 230, 40, 255}, cellW, cellH))

	// Hold the connection open so the page can be looked at, and so a second
	// sequence can be asked for by reloading.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func gridImage() image.Image {
	m := image.NewRGBA(image.Rect(0, 0, width, height))
	for row := 0; row < 3; row++ {
		for col := 0; col < 4; col++ {
			rect := image.Rect(col*cellW, row*cellH, (col+1)*cellW, (row+1)*cellH)
			fill(m, rect, grid[row][col])
		}
	}
	return m
}

func solid(c color.RGBA, w, h int) image.Image {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	fill(m, m.Bounds(), c)
	return m
}

func fill(m *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			m.SetRGBA(x, y, c)
		}
	}
}

func frame(seq uint32, full bool, x, y, w, h int, img image.Image) ([]byte, error) {
	var payload bytes.Buffer
	if err := jpeg.Encode(&payload, img, &jpeg.Options{Quality: 100}); err != nil {
		return nil, err
	}

	out := make([]byte, headerLen+payload.Len())
	out[0] = typeImage
	out[1] = formatJPEG
	var flags uint16
	if full {
		flags |= flagFull
	}
	binary.LittleEndian.PutUint16(out[2:], flags)
	binary.LittleEndian.PutUint16(out[4:], uint16(x))
	binary.LittleEndian.PutUint16(out[6:], uint16(y))
	binary.LittleEndian.PutUint16(out[8:], uint16(w))
	binary.LittleEndian.PutUint16(out[10:], uint16(h))
	binary.LittleEndian.PutUint32(out[12:], seq)
	binary.LittleEndian.PutUint32(out[16:], uint32(payload.Len()))
	copy(out[headerLen:], payload.Bytes())
	return out, nil
}
