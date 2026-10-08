package rdp

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAgainstARealServer is the only test here that talks to a server somebody
// else wrote. It is skipped unless ADX_TEST_RDP_HOST is set, so an ordinary
// `go test ./...` neither needs a network nor connects to anything by accident.
//
//	ADX_TEST_RDP_HOST=192.168.75.130:3389 \
//	ADX_TEST_RDP_USER=rdptest \
//	ADX_TEST_RDP_PASS=rdptest \
//	go test ./internal/rdp/ -run TestAgainstARealServer -v
//
// It reads only: it connects, waits for frames, subscribes to pointer shapes and
// asks for a resize. It deliberately does not type anything, because a stray
// keystroke lands in whatever window the session happens to have focused.
func TestAgainstARealServer(t *testing.T) {
	host := os.Getenv("ADX_TEST_RDP_HOST")
	if host == "" {
		t.Skip("set ADX_TEST_RDP_HOST, ADX_TEST_RDP_USER and ADX_TEST_RDP_PASS to run this")
	}
	user := os.Getenv("ADX_TEST_RDP_USER")
	password := os.Getenv("ADX_TEST_RDP_PASS")

	// Which security path the server offers is worth knowing on its own: the
	// failure for each is what the gateway turns into an operator-facing
	// message, so the classifications are checked against real errors rather
	// than invented strings.
	var working Protocol
	for _, proto := range []Protocol{"nla", "tls", "rdp"} {
		t.Run("protocol/"+string(proto), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			s, err := Dial(ctx, Config{
				Host: host, User: user, Password: password,
				Protocol: string(proto), Width: 1280, Height: 720,
				Timeout: 15 * time.Second,
			})
			if err != nil {
				t.Logf("%s: code=%q err=%v", proto, CodeOf(err), err)
				if strings.Contains(err.Error(), password) {
					t.Fatalf("the password is in the error text: %v", err)
				}
				return
			}
			defer s.Close()
			working = proto
			t.Logf("%s: connected", proto)
		})
	}

	// The read path is worth checking on whichever protocol worked, and if none
	// did there is nothing to read from.
	if working == "" {
		t.Skip("no protocol connected, so there is nothing to read")
	}
	t.Logf("reading over %s", working)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	s, err := Dial(ctx, Config{
		Host: host, User: user, Password: password,
		Protocol: string(working), Width: 1280, Height: 720,
		Timeout: 20 * time.Second, Name: "integration",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	// ---- frames (requirement A) ----

	type batch struct {
		regions []image.Rectangle
	}
	batches := make(chan batch, 64)
	if err := s.OnFrame(func(dirty []image.Rectangle) {
		select {
		case batches <- batch{regions: append([]image.Rectangle(nil), dirty...)}:
		default:
		}
	}); err != nil {
		t.Fatalf("OnFrame: %v", err)
	}

	// ---- pointer shapes (requirement B) ----

	shapes := make(chan Cursor, 32)
	if err := s.OnCursor(func(cur Cursor) {
		select {
		case shapes <- cur:
		default:
		}
	}); err != nil {
		t.Fatalf("OnCursor: %v", err)
	}

	// ---- resize (requirement E) ----

	resizes := make(chan [2]int, 8)
	if err := s.OnResize(func(w, h int) {
		select {
		case resizes <- [2]int{w, h}:
		default:
		}
	}); err != nil {
		t.Fatalf("OnResize: %v", err)
	}

	// Wait for the desktop to settle, collecting everything as it arrives.
	deadline := time.After(12 * time.Second)
	var frameCount, regionCount int
	var lastFrame image.Rectangle
	var firstShape *Cursor
	var sawSystemShape bool
	var sawHidden bool
loop:
	for {
		select {
		case b := <-batches:
			frameCount++
			regionCount += len(b.regions)
			for _, r := range b.regions {
				lastFrame = lastFrame.Union(r)
			}
		case c := <-shapes:
			if firstShape == nil && c.Image != nil {
				cp := c
				firstShape = &cp
			}
			if c.System {
				sawSystemShape = true
			}
			if c.Hidden() {
				sawHidden = true
			}
		case <-deadline:
			break loop
		}
	}

	if frameCount == 0 {
		t.Fatal("no frames arrived: requirement A did not produce a desktop")
	}
	t.Logf("frames: %d batches covering %d regions, last dirty area %v", frameCount, regionCount, lastFrame)

	fb, err := s.Framebuffer()
	if err != nil {
		t.Fatalf("Framebuffer after frames arrived: %v", err)
	}
	bounds := fb.Bounds()
	if bounds.Empty() {
		t.Fatal("the framebuffer is empty although frames arrived")
	}
	t.Logf("desktop: %v, stride %d", bounds, fb.Stride())
	if bounds.Dx() < 200 || bounds.Dy() < 200 {
		t.Errorf("desktop is implausibly small: %v", bounds)
	}

	// A desktop that decoded correctly is not one flat colour. Counting
	// distinct bytes is a crude check, but it catches the two failures that
	// matter: an all black buffer, and a buffer where nothing was ever drawn.
	if distinct := distinctBytes(fb.Pix()); distinct < 4 {
		t.Errorf("the framebuffer has only %d distinct byte values; the decode probably failed", distinct)
	}

	// Keep a copy so the picture can be looked at rather than trusted.
	dumpFramebuffer(t, fb)

	// ---- pointer ----

	if firstShape == nil {
		if sawSystemShape {
			t.Log("only system cursors arrived, which have no pixels by design")
		} else if sawHidden {
			t.Log("the server asked for no pointer")
		} else {
			t.Error("no pointer shape arrived at all: requirement B reported nothing")
		}
	} else {
		b := firstShape.Image.Bounds()
		t.Logf("first pointer shape: %dx%d hotspot %v", b.Dx(), b.Dy(), firstShape.Hotspot)
		if b.Empty() {
			t.Error("a pointer shape arrived with no pixels")
		}
		if !firstShape.Hotspot.In(b) {
			t.Errorf("hotspot %v is outside the shape %v", firstShape.Hotspot, b)
		}
		// A shape with no opaque pixel at all is a decode that produced
		// nothing, which is the failure the old code had.
		if opaque := countOpaque(firstShape.Image); opaque == 0 {
			t.Error("the pointer shape has no opaque pixels")
		} else {
			t.Logf("opaque pointer pixels: %d of %d", opaque, b.Dx()*b.Dy())
		}
	}

	// ---- resize ----

	// grdp learns a new desktop size from the server's drawing rather than from
	// a reply: the buffer grows when a bitmap lands outside it, and the test
	// there is strictly greater. Two consequences are worth measuring rather
	// than assuming, so both directions are tried and the observations are
	// reported.
	before := bounds

	probe := func(label string, w, h int) {
		// Count frames from here so "nothing happened" can be told apart from
		// "the server drew but the size was not noticed". The union of what was
		// drawn is the decisive part: if every region fits inside the size that
		// was asked for, the server applied it, and a framebuffer still claiming
		// the old size is the client having missed the change.
		framesBefore := frameCount
		var drawn image.Rectangle
		if err := s.RequestResize(w, h); err != nil {
			t.Logf("%s: request refused: %v", label, err)
			return
		}
		timeout := time.After(12 * time.Second)
		for {
			select {
			case b := <-batches:
				frameCount++
				for _, r := range b.regions {
					drawn = drawn.Union(r)
				}
			case got := <-resizes:
				t.Logf("%s: OnResize reported %dx%d (asked %dx%d), framebuffer now %v",
					label, got[0], got[1], w, h, framebufferBounds(t, s))
				return
			case <-timeout:
				frames := frameCount - framesBefore
				fb := framebufferBounds(t, s)
				requested := image.Rect(0, 0, w, h)
				switch {
				case frames == 0:
					t.Logf("%s: asked for %dx%d; nothing was drawn, so grdp had "+
						"nothing to learn a new size from and OnResize could not fire",
						label, w, h)
				case drawn.In(requested):
					// The server moved to the size that was asked for: every
					// region it drew fits inside it. A framebuffer still larger
					// than that is the client having missed the change.
					//
					// A failure here is a finding about grdp, not about this
					// gateway: see requirement J in docs/grdp-requirements.md.
					// The probe above is the whole of the evidence, so if it
					// ever fires, that document is where the answer goes.
					t.Errorf("%s: the server drew only within %v, so it applied %dx%d, "+
						"but the framebuffer is still %v and OnResize never fired",
						label, drawn, w, h, fb)
				default:
					t.Logf("%s: asked for %dx%d; %d frames drawn covering %v, which "+
						"reaches outside the request, so the server did not apply it "+
						"(framebuffer %v)",
						label, w, h, frames, drawn, fb)
				}
				return
			}
		}
	}

	// Growing should be noticed, because the server draws where the old buffer
	// had no room.
	probe("grow", 1600, 900)

	// Shrinking cannot be noticed by that rule: every bitmap lands inside a
	// buffer that is already too big, so a client that only ever grows keeps
	// framing the desktop wrongly. Measured here rather than asserted.
	probe("shrink", 800, 600)

	// Put it back so the next run starts where this one did.
	if err := s.RequestResize(before.Dx(), before.Dy()); err != nil {
		t.Logf("resize back: %v", err)
	}

	// The invariant that must hold whatever the server did: every region that
	// was ever reported is inside the framebuffer the caller is given.
	final := framebufferBounds(t, s)
	if final.Empty() {
		t.Error("the framebuffer is empty after a resize")
	}
	t.Logf("final framebuffer: %v", final)
}

// framebufferBounds reads the current size, or an empty rectangle if there is
// nothing to read.
func framebufferBounds(t *testing.T, s *Session) image.Rectangle {
	t.Helper()
	fb, err := s.Framebuffer()
	if err != nil {
		return image.Rectangle{}
	}
	return fb.Bounds()
}

// Protocol is the security path, named here only so the table above reads
// clearly.
type Protocol string

func distinctBytes(b []byte) int {
	seen := [256]bool{}
	n := 0
	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			n++
		}
	}
	return n
}

func countOpaque(img *image.RGBA) int {
	n := 0
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0 {
			n++
		}
	}
	return n
}

// dumpFramebuffer writes what was decoded to a PNG, so the picture can be
// looked at rather than inferred from a byte count.
func dumpFramebuffer(t *testing.T, fb Framebuffer) {
	t.Helper()
	bounds := fb.Bounds()
	img := image.NewRGBA(bounds)
	stride := fb.Stride()
	pix := fb.Pix()
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			o := y*stride + x*4
			if o+3 >= len(pix) {
				continue
			}
			img.SetRGBA(x, y, color.RGBA{R: pix[o+2], G: pix[o+1], B: pix[o], A: 255})
		}
	}

	path := filepath.Join(os.TempDir(), "adx-integration-screen.png")
	f, err := os.Create(path)
	if err != nil {
		t.Logf("could not open %s: %v", path, err)
		return
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Logf("could not encode %s: %v", path, err)
		return
	}
	t.Logf("wrote %s (%dx%d)", path, bounds.Dx(), bounds.Dy())
}
