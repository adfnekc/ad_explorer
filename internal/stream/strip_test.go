package stream

import (
	"image"
	"testing"
)

func TestStripsCoverTheRegionExactlyOnce(t *testing.T) {
	full := makeDesktop(1920, 1080)
	acc := NewAccumulator(full.Bounds(), DefaultMaxRects, DefaultFullFraction)
	acc.Add(full.Bounds())
	frames, err := NewBuilder(nil).Build(full, acc, 85)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 2 {
		t.Fatalf("a whole desktop came back as %d frame(s), so it was not divided", len(frames))
	}
	// 序号连续，只有第一片带 FlagFull
	for i, f := range frames {
		if f.Seq != uint32(i+1) {
			t.Errorf("frame %d has seq %d", i, f.Seq)
		}
		want := i == 0
		if f.IsFull() != want {
			t.Errorf("frame %d IsFull=%v, want %v", i, f.IsFull(), want)
		}
	}
	// 条带恰好覆盖整屏一次，无重叠无空隙
	seen := 0
	y := 0
	for _, f := range frames {
		if f.Rect.Min.X != 0 || f.Rect.Max.X != 1920 {
			t.Errorf("strip %v is not full width", f.Rect)
		}
		if f.Rect.Min.Y != y {
			t.Errorf("strip starts at %d, the one before ended at %d", f.Rect.Min.Y, y)
		}
		y = f.Rect.Max.Y
		seen += f.Rect.Dx() * f.Rect.Dy()
	}
	if y != 1080 {
		t.Errorf("strips end at %d, want 1080", y)
	}
	if seen != 1920*1080 {
		t.Errorf("strips cover %d pixels, want %d", seen, 1920*1080)
	}
}

func TestASmallRegionIsNotDivided(t *testing.T) {
	full := makeDesktop(1920, 1080)
	acc := NewAccumulator(full.Bounds(), DefaultMaxRects, DefaultFullFraction)
	small := image.Rect(100, 100, 400, 300)
	acc.Add(small)
	frames, err := NewBuilder(nil).Build(full, acc, 85)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Rect != small {
		t.Fatalf("a small region came back as %d frames: %v", len(frames), frames)
	}
}
