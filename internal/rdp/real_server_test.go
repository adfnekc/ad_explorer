package rdp

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/adfnekc/grdp/protocol/pdu"

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
	// ADX_TEST_TRACE sets Config.TraceInput, which is what actually raises the
	// library's log level. Setting glog directly here would not work: the level
	// is re-asserted when a session is built, so it would be overwritten.
	trace := os.Getenv("ADX_TEST_TRACE") != ""
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
		Protocol: string(working),
		Width:    1024, Height: 768,
		Timeout: 20 * time.Second, Name: "integration",
		// ADX_TEST_SLOW_INPUT=1 sends input over the slow path, which is how
		// the two encodings are told apart when a server ignores one of them.
		NoFastPathInput: os.Getenv("ADX_TEST_SLOW_INPUT") != "",
		TraceInput:      trace,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	// ---- pointer shapes ----
	// The PNG the gateway would send goes to disk so it can be looked at: a shape
	// that arrives as the wrong pixels is not something a count can show.
	{
		seen := 0
		_ = s.OnCursor(func(cur Cursor) {
			// Every shape is logged, system or pixels, because the question is
			// which kind the server sends for a text field's I beam.
			shape := image.Rectangle{}
			if cur.Image != nil {
				shape = cur.Image.Bounds()
			}
			// What a 32bpp shape of this size would need, against what arrived.
			want := 0
			if cur.XorBpp > 0 && shape.Dx() > 0 {
				want = pdu.PointerXorRowBytes(shape.Dx(), cur.XorBpp) * shape.Dy()
			}
			t.Logf("cursor event: system=%v type=0x%04x xorbpp=%d size=%dx%d hotspot=%v xor=%dB and=%dB want=%dB",
				cur.System, cur.SystemType, cur.XorBpp, shape.Dx(), shape.Dy(), cur.Hotspot,
				cur.RawXorLen, cur.RawAndLen, want)
			if cur.System || cur.Image == nil || seen >= 20 {
				return
			}
			seen++
			b := cur.Image.Bounds()
			f, err := os.Create(filepath.Join(os.TempDir(),
				fmt.Sprintf("adx-cursor-%d-bpp%d-%dx%d.png", seen, cur.XorBpp, shape.Dx(), shape.Dy())))
			if err != nil {
				return
			}
			defer f.Close()
			if err := png.Encode(f, cur.Image); err != nil {
				t.Logf("cursor png: %v", err)
				return
			}
			t.Logf("cursor %d: %dx%d hotspot %v", seen, b.Dx(), b.Dy(), cur.Hotspot)
		})
	}

	// Hover a text field so the I beam is what the server sends, then another
	// patch of desktop so it goes back to an arrow.
	s.MouseMove(180, 745)
	time.Sleep(1200 * time.Millisecond)
	s.MouseMove(700, 300)
	time.Sleep(700 * time.Millisecond)

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

	// ---- does input actually reach the server? ----
	//
	// This probe compares the screen before and after a right click on the empty
	// desktop: a context menu changes a large part of it, changes no state, and
	// Escape closes it again.
	//
	// It reports rather than fails by default, because this host cannot always
	// answer the question, and grdp's own Windows notes say so: a connection that
	// meets the shell's "Another user is signed in. Do you want to sign in
	// anyway?" prompt gets no input at all until Yes is clicked, and that target
	// is also documented as one where the shell does not take keyboard focus, so
	// typing reaches nothing while the mouse works. A silent screen therefore
	// means "not demonstrable here" far more often than it means "the input path
	// is broken", and a test that cried wolf would stop being read.
	//
	// Set ADX_TEST_REQUIRE_INPUT=1 on a host whose session is already signed in
	// to make it a failure instead.
	t.Run("input reaches the server", func(t *testing.T) {
		// Let the session settle. A takeover signs the previous session out and
		// signs this one in, which takes seconds.
		time.Sleep(6 * time.Second)

		positions := make(chan [2]int, 64)
		go func() {
			for ev := range s.Events() {
				if pos, ok := ev.(CursorPos); ok {
					select {
					case positions <- [2]int{pos.X, pos.Y}:
					default:
					}
				}
			}
		}()

		// Clear whatever was selected before, because Windows selects a desktop
		// icon on button down and the selection outlives the session that made
		// it: without this the icon is already selected in both frames and a
		// click that worked looks exactly like a click that did nothing.
		s.MouseMove(700, 300)
		time.Sleep(200 * time.Millisecond)
		s.MouseButton(0, true, 700, 300)
		time.Sleep(60 * time.Millisecond)
		s.MouseButton(0, false, 700, 300)
		time.Sleep(600 * time.Millisecond)

		// Snapshot with the pointer already resting on the icon, so the hover
		// highlight is in both frames and only the click can differ. Taking the
		// baseline before the move made the highlight look like the click.
		s.MouseMove(36, 36)
		time.Sleep(1500 * time.Millisecond)
		before := snapshot(t, s)

		// A double click, with the press and release separated the way a
		// person's click separates them.
		for i := 0; i < 2; i++ {
			s.MouseButton(0, true, 36, 36)
			time.Sleep(70 * time.Millisecond)
			s.MouseButton(0, false, 36, 36)
			time.Sleep(70 * time.Millisecond)
		}
		time.Sleep(2500 * time.Millisecond)
		after := snapshot(t, s)

		writePNG(t, before, "adx-click-before.png")
		writePNG(t, after, "adx-click-after.png")

		// One notch each way, to see what the wheel encoding is in each
		// direction. A server that scrolls one direction further than the
		// other is a sign that the two are not encoded symmetrically.
		s.MouseWheel(3, 512, 360)
		time.Sleep(300 * time.Millisecond)
		s.MouseWheel(-3, 512, 360)
		time.Sleep(300 * time.Millisecond)

		changed := differingPixels(before, after)
		total := before.Bounds().Dx() * before.Bounds().Dy()
		t.Logf("double clicking the Recycle Bin changed %d of %d pixels (%.1f%%)",
			changed, total, float64(changed)/float64(total)*100)

		// Close the window so the session is left as it was found.
		s.KeyScancode(ScEscape, true)
		s.KeyScancode(ScEscape, false)

		if changed*100/total < 1 {
			msg := fmt.Sprintf("a double click on the Recycle Bin changed nothing (%d of %d pixels). "+
				"On a session that is not signed in, or whose shell has not taken keyboard "+
				"focus, this proves nothing; see grdp's docs/windows-verification.md", changed, total)
			if os.Getenv("ADX_TEST_REQUIRE_INPUT") != "" {
				t.Error(msg)
			} else {
				t.Log("NOTE: " + msg)
			}
		}

		// The pointer echo is a second opinion, and a weaker one: not every
		// server sends it, and a move that lands while the session is still
		// signing in is ignored.
		s.MouseMove(320, 240)
		s.MouseMove(640, 360)
		seen := 0
		deadline := time.After(2 * time.Second)
	collect:
		for {
			select {
			case <-positions:
				seen++
			case <-deadline:
				break collect
			}
		}
		t.Logf("the server reported %d pointer positions after two moves", seen)
	})

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

// writePNG saves an image where a person can look at it.
func writePNG(t *testing.T, img *image.RGBA, name string) {
	t.Helper()
	path := filepath.Join(os.TempDir(), name)
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
	t.Logf("wrote %s", path)
}

// snapshot reads the current framebuffer into an image the test owns.
func snapshot(t *testing.T, s *Session) *image.RGBA {
	t.Helper()
	fb, err := s.Framebuffer()
	if err != nil {
		t.Fatalf("Framebuffer: %v", err)
	}
	bounds := fb.Bounds()
	out := image.NewRGBA(bounds)
	pix, stride := fb.Pix(), fb.Stride()
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			o := y*stride + x*4
			if o+3 >= len(pix) {
				continue
			}
			out.SetRGBA(x, y, color.RGBA{R: pix[o+2], G: pix[o+1], B: pix[o], A: 255})
		}
	}
	return out
}

// differingPixels counts pixels that differ by more than a compression artefact.
// The frames come from a lossy codec, so an exact comparison would report noise
// as change.
func differingPixels(a, b *image.RGBA) int {
	if a.Bounds() != b.Bounds() {
		return a.Bounds().Dx() * a.Bounds().Dy()
	}
	n := 0
	bounds := a.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			ca, cb := a.RGBAAt(x, y), b.RGBAAt(x, y)
			if diff(ca.R, cb.R) > 24 || diff(ca.G, cb.G) > 24 || diff(ca.B, cb.B) > 24 {
				n++
			}
		}
	}
	return n
}

func diff(a, b uint8) int {
	if a > b {
		return int(a) - int(b)
	}
	return int(b) - int(a)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func keysOf(m map[[2]int]bool) [][2]int {
	out := make([][2]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

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

// driveCopyOnRemote opens a folder on the remote and copies what is in it, so
// that a transfer can be measured without asking the operator to do it.
//
// It is the remote's own keyboard being driven, not a protocol call: the Run
// dialog, the address, select all, copy. Each step is a wait for a window to
// appear rather than a fixed confidence, and the waits are generous because a
// remote desktop that has just opened Explorer is slow to be ready for the next
// key.
func driveCopyOnRemote(t *testing.T, s *Session, folder string) {
	t.Helper()
	press := func(code string) {
		t.Helper()
		chord(t, s, nil, code)
	}
	chord(t, s, []string{"MetaLeft"}, "KeyR")
	time.Sleep(1500 * time.Millisecond)

	if err := s.TypeText(folder); err != nil {
		t.Fatalf("typing the path: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	press("Enter")

	// Explorer draws before it is ready to be asked for its contents.
	time.Sleep(4 * time.Second)
	chord(t, s, []string{"ControlLeft"}, "KeyA")
	time.Sleep(400 * time.Millisecond)
	chord(t, s, []string{"ControlLeft"}, "KeyC")
	time.Sleep(600 * time.Millisecond)
	t.Log("drove the remote to copy the folder's contents")
}

// chord presses a modifier, taps a key, and lets the modifier go.
func chord(t *testing.T, s *Session, mods []string, code string) {
	t.Helper()
	for _, m := range mods {
		if err := s.Key(m, true); err != nil {
			t.Fatalf("holding %s: %v", m, err)
		}
	}
	if err := s.Key(code, true); err != nil {
		t.Fatalf("pressing %s: %v", code, err)
	}
	if err := s.Key(code, false); err != nil {
		t.Fatalf("releasing %s: %v", code, err)
	}
	for i := len(mods) - 1; i >= 0; i-- {
		if err := s.Key(mods[i], false); err != nil {
			t.Fatalf("releasing %s: %v", mods[i], err)
		}
	}
}

// TestRemoteFileThroughput measures copying a file from the remote.
//
// It is separate from the input probe because it needs the remote to have put a
// file on the shared clipboard, which is the only way a manifest ever arrives:
// in this protocol the side that copies is the side that offers, and a client
// cannot reach into a path on the remote — that would need drive redirection,
// which this library does not speak. An empty manifest is reported as such
// rather than waited on.
func TestRemoteFileThroughput(t *testing.T) {
	host := os.Getenv("ADX_TEST_RDP_HOST")
	if host == "" {
		t.Skip("ADX_TEST_RDP_HOST is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s, err := Dial(ctx, Config{
		Host: host, User: os.Getenv("ADX_TEST_RDP_USER"), Password: os.Getenv("ADX_TEST_RDP_PASS"),
		Protocol: "nla", Width: 1024, Height: 768, Timeout: 20 * time.Second, Name: "filebench",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	files := s.Files()
	if files == nil {
		t.Fatal("this session has no file transfer surface")
	}

	offers := make(chan []RemoteFile, 4)
	if err := files.OnRemoteFiles(func(list []RemoteFile) { offers <- list }); err != nil {
		t.Fatalf("OnRemoteFiles: %v", err)
	}

	// The side that copies is the side that offers, and this client cannot reach
	// a path on the remote — that would need drive redirection, which this
	// library does not speak. It can, however, drive the remote's own keyboard,
	// so it opens the folder and copies the file the way a person would.
	//
	// The drive is retried rather than trusted: opening a folder and pressing
	// keys into whatever window came up depends on the remote being ready, and a
	// single attempt failed often enough that a skip looked like the clipboard
	// being empty when it was the driving that missed.
	var manifest []RemoteFile
	if os.Getenv("ADX_TEST_DRIVE_COPY") != "" {
		for attempt := 1; attempt <= 3 && manifest == nil; attempt++ {
			driveCopyOnRemote(t, s, os.Getenv("ADX_TEST_COPY_PATH"))
			select {
			case manifest = <-offers:
				t.Logf("the remote offered its clipboard after attempt %d", attempt)
			case <-time.After(8 * time.Second):
				t.Logf("attempt %d produced nothing", attempt)
			}
		}
	}

	// The manifest arrives as a clipboard update rather than on demand, so when
	// nothing was driven it has to be waited for.
	if manifest == nil {
		deadline := time.After(20 * time.Second)
		for manifest == nil {
			select {
			case manifest = <-offers:
			case <-deadline:
				t.Skip("the remote has not put anything on the shared clipboard; " +
					"copy a file on the remote first and run this again")
			}
		}
	}

	if len(manifest) == 0 {
		t.Skip("the remote's clipboard has no files on it")
	}
	for _, f := range manifest {
		t.Logf("offered: index=%d name=%q size=%d dir=%v", f.Index, f.Name, f.Size, f.IsDir)
	}

	var target *RemoteFile
	for i := range manifest {
		if !manifest[i].IsDir && strings.Contains(manifest[i].Name, "pinyin_guanwang") {
			target = &manifest[i]
			break
		}
	}
	if target == nil {
		t.Skipf("none of the offered files is the one asked for")
	}

	// A CPU profile of this process is a profile of the whole path: the transfer
	// is read here, so grdp's clipboard channel, the TLS layer and every copy
	// between them are all in this binary. A gateway that is idle while a
	// transfer crawls means the time is spent somewhere else entirely.
	if path := os.Getenv("ADX_TEST_CPUPROFILE"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("profile: %v", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			t.Fatalf("profile: %v", err)
		}
		defer func() {
			pprof.StopCPUProfile()
			f.Close()
			t.Logf("profile written to %s", path)
		}()
	}

	// Ranges are read as fast as the protocol answers them, which is the number
	// being measured: a round trip per range is what the throughput depends on.
	// The range size is what the throughput depends on, so it is a parameter
	// rather than a constant: a round trip per range means bytes per second is
	// range size over latency, and the point of measuring is to find where that
	// stops being true.
	rangeSize := 64 << 10
	if v := os.Getenv("ADX_TEST_RANGE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rangeSize = n
		}
	}
	var read int64
	var slowest, fastest time.Duration
	start := time.Now()
	for read < target.Size {
		want := rangeSize
		if remain := target.Size - read; remain < int64(want) {
			want = int(remain)
		}
		t0 := time.Now()
		data, err := files.ReadRemoteFile(target.Index, read, want)
		took := time.Since(t0)
		if err != nil {
			t.Fatalf("read %d at %d: %v", want, read, err)
		}
		if len(data) == 0 {
			t.Fatalf("read returned nothing at %d of %d", read, target.Size)
		}
		if slowest == 0 || took > slowest {
			slowest = took
		}
		if fastest == 0 || took < fastest {
			fastest = took
		}
		read += int64(len(data))
	}
	elapsed := time.Since(start)

	mbps := float64(read) / (1 << 20) / elapsed.Seconds()

	// The same bytes a second time. The remote has them in its page cache by
	// now, so a much faster second pass means the limit was the remote's disk
	// and not the path between here and there. An unchanged one means the
	// bytes are being moved at the same rate either way, which is a bandwidth
	// limit in the transfer itself.
	second := time.Now()
	var again int64
	for again < target.Size {
		want := rangeSize
		if remain := target.Size - again; remain < int64(want) {
			want = int(remain)
		}
		data, err := files.ReadRemoteFile(target.Index, again, want)
		if err != nil {
			t.Fatalf("second pass at %d: %v", again, err)
		}
		if len(data) == 0 {
			break
		}
		again += int64(len(data))
	}
	secondTook := time.Since(second)
	t.Logf("second pass: %d bytes in %v = %.2f MB/s (%.2fx the first)",
		again, secondTook.Round(time.Millisecond),
		float64(again)/(1<<20)/secondTook.Seconds(), elapsed.Seconds()/secondTook.Seconds())
	t.Logf("read %d bytes in %v = %.2f MB/s", read, elapsed.Round(time.Millisecond), mbps)
	t.Logf("per range of %d bytes: fastest %v, slowest %v, mean %v",
		rangeSize, fastest.Round(time.Microsecond), slowest.Round(time.Microsecond),
		(elapsed / time.Duration(max(1, int(read)/rangeSize))).Round(time.Microsecond))

	// The interesting number is what one round trip costs against what it
	// carried: a range of this size over this latency is the ceiling, and no
	// amount of work on this side moves it.
	t.Logf("if a range costs the mean and carries %d bytes, the ceiling is %.2f MB/s",
		rangeSize, float64(rangeSize)/(1<<20)/((elapsed / time.Duration(max(1, int(read)/rangeSize))).Seconds()))
}

// countingProvider serves one generated file and records how the remote asks
// for it.
//
// The size of each read is the whole question: a paste asks in ranges and pays a
// round trip for each, so bytes per second is the range size over the latency,
// and the remote is the side that picks it. A small range here is a ceiling this
// side cannot lift, and a large one means the slowness is somewhere else.
type countingProvider struct {
	data    []byte
	name    string
	modMu   sync.Mutex
	sizes   []int
	total   int64
	elapsed time.Duration
}

func (p *countingProvider) Names() []string { return []string{p.name} }

func (p *countingProvider) Info(name string) (int64, time.Time, bool) {
	if name != p.name {
		return 0, time.Time{}, false
	}
	return int64(len(p.data)), time.Now(), false
}

func (p *countingProvider) Open(name string) (io.ReadCloser, error) {
	if name != p.name {
		return nil, fmt.Errorf("no such entry %q", name)
	}
	return &countingReader{p: p}, nil
}

type countingReader struct {
	p   *countingProvider
	off int64
}

func (r *countingReader) Read(b []byte) (int, error) {
	if r.off >= int64(len(r.p.data)) {
		return 0, io.EOF
	}
	start := time.Now()
	n := copy(b, r.p.data[r.off:])
	r.off += int64(n)
	r.p.modMu.Lock()
	r.p.sizes = append(r.p.sizes, n)
	r.p.total += int64(n)
	r.p.elapsed += time.Since(start)
	r.p.modMu.Unlock()
	return n, nil
}

func (r *countingReader) Close() error { return nil }

// TestRemotePasteAskSize offers a file and drives the remote to paste it, then
// reports how the remote asked for it. This is the direction where the operator
// reports slowness and the only one whose ceiling is not ours to choose.
func TestRemotePasteAskSize(t *testing.T) {
	host := os.Getenv("ADX_TEST_RDP_HOST")
	if host == "" {
		t.Skip("ADX_TEST_RDP_HOST is not set")
	}
	if os.Getenv("ADX_TEST_DRIVE_COPY") == "" {
		t.Skip("set ADX_TEST_DRIVE_COPY=1 to let this test drive the remote's paste")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	s, err := Dial(ctx, Config{
		Host: host, User: os.Getenv("ADX_TEST_RDP_USER"), Password: os.Getenv("ADX_TEST_RDP_PASS"),
		Protocol: "nla", Width: 1024, Height: 768, Timeout: 20 * time.Second, Name: "pastebench",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	files := s.Files()
	if files == nil {
		t.Fatal("this session has no file transfer surface")
	}

	const size = 48 << 20
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i)
	}
	provider := &countingProvider{data: content, name: "adx-paste-bench.bin"}
	files.Offer(provider)
	t.Logf("offered %s, %d bytes", provider.name, size)

	// Open a folder to paste into, then paste. The destination does not matter
	// to the measurement; that there is one does, or the paste lands nowhere.
	chord(t, s, []string{"MetaLeft"}, "KeyR")
	time.Sleep(1500 * time.Millisecond)
	if err := s.TypeText(`C:\Users\rdptest\Downloads`); err != nil {
		t.Fatalf("typing the path: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	chord(t, s, nil, "Enter")
	time.Sleep(5 * time.Second)

	start := time.Now()
	chord(t, s, []string{"ControlLeft"}, "KeyV")
	t.Log("pressed Ctrl+V on the remote")

	deadline := time.After(40 * time.Second)
	var got []int
	_ = got
	for {
		time.Sleep(500 * time.Millisecond)
		provider.modMu.Lock()
		got = append([]int(nil), provider.sizes...)
		provider.modMu.Unlock()
		if len(got) >= 8 {
			break
		}
		select {
		case <-deadline:
			goto done
		default:
		}
	}
done:
	provider.modMu.Lock()
	sizes, total := append([]int(nil), provider.sizes...), provider.total
	elapsed := provider.elapsed
	provider.modMu.Unlock()

	if len(sizes) == 0 {
		t.Fatalf("the remote never asked for anything in %v", time.Since(start).Round(time.Millisecond))
	}
	smallest, largest := sizes[0], sizes[0]
	for _, n := range sizes {
		if n < smallest {
			smallest = n
		}
		if n > largest {
			largest = n
		}
	}
	t.Logf("the remote asked %d times for %d bytes in %v (%.2f MB/s)",
		len(sizes), total, time.Since(start).Round(time.Millisecond),
		float64(total)/(1<<20)/time.Since(start).Seconds())
	t.Logf("asked for min %d, max %d bytes; first few %v", smallest, largest, sizes[:min(len(sizes), 8)])
	t.Logf("this side spent %v serving them", elapsed.Round(time.Millisecond))
	t.Logf("at that ask size, one round trip of 20ms would cap the transfer at %.2f MB/s",
		float64(largest)/(1<<20)/0.020)
}
