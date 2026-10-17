package stream

import (
	"image"
	"image/color"
	"testing"
)

// screenish builds a frame closer to a real desktop: large flat areas, a few
// bordered panels, and detail that is regular rather than random. A PNG of
// random pixels is the worst case there is and says nothing about a screen.
func screenish(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bg := color.RGBA{32, 34, 40, 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, bg)
		}
	}
	// Window: flat body with a title bar and a line of "text".
	for y := 100; y < 500; y++ {
		for x := 200; x < 900; x++ {
			if y < 130 {
				img.SetRGBA(x, y, color.RGBA{60, 90, 160, 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{240, 240, 240, 255})
			}
		}
	}
	for y := 160; y < 460; y += 18 {
		for x := 220; x < 860; x += 2 {
			img.SetRGBA(x, y, color.RGBA{20, 20, 20, 255})
		}
	}
	return img
}

// TestAPNGOfADesktopStaysSmall is a regression guard on how much a frame costs
// to carry. It was written because a benchmark's bytes-per-op was misread as the
// size of a frame, which made the wire look like the bottleneck when it is three
// hundred kilobytes a second. The numbers here are the ones that decide that.
func TestAPNGOfADesktopStaysSmall(t *testing.T) {
	for _, c := range []struct {
		name string
		img  *image.RGBA
	}{
		{"noise 1280x720", makeDesktop(1280, 720)},
		{"desktop-like 1280x720", screenish(1280, 720)},
		{"desktop-like 400x250", screenish(1280, 720).SubImage(image.Rect(200, 150, 600, 400)).(*image.RGBA)},
	} {
		b, _, err := (PNG{}).Encode(nil, c.img, 70)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%-24s %8.1f KB  (raw %6.1f KB)", c.name, float64(len(b))/1024, float64(len(c.img.Pix))/1024)
		if c.name == "desktop-like 1280x720" && len(b) > 64*1024 {
			t.Errorf("a desktop-like frame encoded to %d bytes; the wire is not the bottleneck and should not become one", len(b))
		}
	}
}
