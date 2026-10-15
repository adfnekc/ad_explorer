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
	// flagBatch is FLAG_BATCH_END: this redraw is whole and may be shown.
	flagBatch = 1 << 2
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
	motion := flag.Bool("motion", false, "serve a moving block in a repeatedly dirtied strip, which is the shape that tears")
	listen := flag.String("listen", "127.0.0.1:18650", "address to serve on")
	flag.Parse()

	if *motion {
		motionListen(*listen)
		return
	}

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
		b, err := frame(seq, full, full, x, y, w, h, img)
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

	// Overlapping regions at offsets that are not zero, which is the shape a
	// dragged window makes: the pixels of the two overlap and the later one has
	// to win there. The first script never put a region anywhere but the origin,
	// so a client that ignored the offset entirely would have passed it.
	//
	// A spans x 40..120, y 40..100. B spans x 80..160, y 70..130.
	send(false, 40, 40, 80, 60, solid(color.RGBA{230, 40, 40, 255}, 80, 60))
	send(false, 80, 70, 80, 60, solid(color.RGBA{40, 40, 230, 255}, 80, 60))

	// Hold the connection open so the page can be looked at, and so a second
	// sequence can be asked for by reloading.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The strip the block travels in, and how far it goes before turning around.
const (
	stripY         = 60
	stripH         = 100
	motionSteps    = 40
	motionErasePad = 4
	motionInterval = 8 * time.Millisecond
)

var (
	motionTrack = color.RGBA{24, 26, 30, 255}
	motionBar   = color.RGBA{232, 72, 32, 255}
)

var (
	motionBarW   = 10
	motionBarH   = stripH - 8
	motionBarTop = stripY + 4
	motionMargin = 6
	motionTravel = width - 2*motionMargin - motionBarW
)

// motionListen serves a block that moves across a horizontal strip.
//
// It exists because the scripted frames above settle into a known final canvas,
// so they say whether the regions were drawn in the right order and not whether
// a screen was made of pieces from more than one moment. That is the failure
// that remains: a strip that is dirtied and sent many times a second, where the
// only thing that tells the frames apart is which moment its pixels came from.
//
// Every frame re-sends the whole strip, cleared and redrawn, the way a real
// source does: the block is at exactly one position in it. So a screen showing
// two blocks at once, or a fragment of one, is a client that presented a mix,
// and that can be told apart from a correct client by reading the canvas back
// rather than by looking at it.
func motionListen(listen string) {
	assets, err := webui.Assets()
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, map[string]any{
			"id": "mock", "name": "mock-motion", "host": "mock:0",
			"width": width, "height": height, "reconnectGraceMs": 0,
		})
	})
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			serveMotion(w, r)
			return
		}
		writeJSON(w, map[string]any{})
	})
	mux.Handle("/", http.FileServerFS(assets))

	log.Printf("mock motion on http://%s/", listen)
	log.Fatal(http.ListenAndServe(listen, mux))
}

// serveMotion keeps sending the strip while the browser is connected, and it
// sends each moment in the order a real source does when something moves: the
// new pixels first and the ones that stop being true afterwards.
//
// That order is the whole point. A source that draws a moving block reveals the
// track behind it and paints the block at its new place, and those two pieces
// are separate updates because they are separate damage. They usually arrive a
// few milliseconds apart, and a client that presents each one as it finishes
// puts the new block on a screen where the old one has not been cleared yet:
// two blocks, which is what a person calls tearing. A client that waits until
// the moment is whole shows one.
func serveMotion(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	defer conn.Close()

	ready, _ := json.Marshal(map[string]any{
		"type": "session.ready", "id": "mock", "width": width, "height": height,
	})
	if err := conn.WriteMessage(websocket.TextMessage, ready); err != nil {
		return
	}

	// The whole desktop, so the strip is always a slice of a real screen rather
	// than a fresh image: what the block was drawn over last moment is still
	// there in it, and the mock has to clear it.
	desktop := motionDesktop()
	seq := uint32(0)
	step := 0

	movement := time.NewTicker(motionInterval)
	defer movement.Stop()
	keepAlive := time.NewTicker(20 * time.Millisecond)
	defer keepAlive.Stop()

	send := func(rect image.Rectangle, img image.Image, batch bool) bool {
		seq++
		b, err := frame(seq, false, batch, rect.Min.X, rect.Min.Y, rect.Dx(), rect.Dy(), img)
		if err != nil {
			log.Printf("frame %d: %v", seq, err)
			return false
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
			return false
		}
		return true
	}

	for {
		select {
		case <-keepAlive.C:
			// Reading without a deadline is what stops a browser that closed
			// its socket from wedging the sender behind an unread message.
			_ = conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			if _, _, err := conn.NextReader(); err != nil && !isTimeout(err) {
				return
			}
			continue
		case <-movement.C:
		}

		prev := motionBarRect(step)
		step = (step + 1) % motionSteps
		next := motionBarRect(step)

		// The new block first and the clearing of the old one second: that is
		// the order a source leaves its damage in when it paints before it
		// cleans up. A client that waits for the whole moment shows one block;
		// a client that shows each piece as it arrives shows the new block on a
		// screen where the old one is still standing.
		if !send(next, motionBarImage(), false) {
			return
		}
		if !send(expand(prev, motionErasePad), motionEraseTrack(desktop, prev, motionErasePad), true) {
			return
		}
	}
}

// motionDesktop is the desktop the strip is taken from, painted once and then
// edited in place, so the block leaves a trace the mock has to clear.
func motionDesktop() *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, width, height))
	fill(m, m.Bounds(), motionTrack)
	return m
}

// motionBarRect is where the block sits at one step.
func motionBarRect(step int) image.Rectangle {
	x := motionMargin + motionTravel*step/motionSteps
	return image.Rect(x, motionBarTop, x+motionBarW, motionBarTop+motionBarH)
}

// expand grows a rectangle by n in every direction, so the pixels an update
// covers are not only the block but a little of what surrounds it.
func expand(r image.Rectangle, n int) image.Rectangle {
	return image.Rect(r.Min.X-n, r.Min.Y-n, r.Max.X+n, r.Max.Y+n)
}

// motionEraseTrack is the region an old block leaves behind: a cleared
// rectangle, drawn by the mock rather than by the client.
func motionEraseTrack(desktop *image.RGBA, r image.Rectangle, pad int) image.Image {
	out := expand(r, pad).Intersect(desktop.Bounds())
	m := image.NewRGBA(out)
	fill(m, m.Bounds(), motionTrack)
	fill(desktop, out, motionTrack)
	return m
}

// motionBarImage is the block on its own.
func motionBarImage() image.Image {
	m := image.NewRGBA(image.Rect(0, 0, motionBarW, motionBarH))
	fill(m, m.Bounds(), motionBar)
	return m
}

func isTimeout(err error) bool { return err != nil && strings.Contains(err.Error(), "timeout") }

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

// frame encodes one message. batchEnd marks the message that finishes a redraw,
// which is what the browser treats as "this screen is whole"; without it the
// client waits for a batch that never closes and the screen stops moving.
func frame(seq uint32, full bool, batchEnd bool, x, y, w, h int, img image.Image) ([]byte, error) {
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
	if full || batchEnd {
		flags |= flagFull | flagBatch
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
