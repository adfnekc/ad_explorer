package stream

import "testing"

func TestQualityClampsItsArguments(t *testing.T) {
	cases := []struct {
		name             string
		min, max, start  int
		wantCur          int
		wantMin, wantMax int
	}{
		{"normal", 30, 90, 60, 60, 30, 90},
		{"start below min", 30, 90, 5, 30, 30, 90},
		{"start above max", 30, 90, 200, 90, 30, 90},
		{"min below 1", 0, 90, 50, 50, 1, 90},
		{"max above 100", 30, 200, 50, 50, 30, 100},
		{"max below min", 80, 20, 50, 80, 80, 80},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewQuality(tc.min, tc.max, tc.start)
			if q.Value() != tc.wantCur {
				t.Errorf("Value() = %d, want %d", q.Value(), tc.wantCur)
			}
			if q.Min() != tc.wantMin || q.Max() != tc.wantMax {
				t.Errorf("bounds = %d..%d, want %d..%d", q.Min(), q.Max(), tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestQualityBacksOffOnlyAfterSustainedDrops(t *testing.T) {
	q := NewQuality(20, 90, 80)

	// A handful of drops is a hiccup, not a saturated link: changing quality
	// for it would make the picture pulse.
	for i := 0; i < DefaultDropLimit-1; i++ {
		q.OnDropped()
	}
	if q.Value() != 80 {
		t.Errorf("backed off after %d drops, want to wait for %d", DefaultDropLimit-1, DefaultDropLimit)
	}

	q.OnDropped()
	if q.Value() != 80-DefaultStep {
		t.Errorf("Value() = %d after %d drops, want one step down to %d",
			q.Value(), DefaultDropLimit, 80-DefaultStep)
	}
}

func TestQualityRecoversSlowly(t *testing.T) {
	q := NewQuality(20, 90, 50)

	// A single delivered frame must reset the drop streak but must not be
	// enough to start climbing back.
	q.OnDropped()
	q.OnSent()
	if q.Value() != 50 {
		t.Errorf("Value() = %d, want it unchanged", q.Value())
	}

	for i := 0; i < DefaultGoodLimit; i++ {
		q.OnSent()
	}
	if q.Value() != 50+DefaultStep/2 {
		t.Errorf("Value() = %d after %d good frames, want %d", q.Value(), DefaultGoodLimit+1, 50+DefaultStep/2)
	}
}

func TestQualityRecoveryTakesLongerThanBackoff(t *testing.T) {
	// The asymmetry is the point: if climbing back were as fast as falling,
	// a marginal link would oscillate for as long as it stayed marginal.
	if DefaultGoodLimit <= DefaultDropLimit {
		t.Fatalf("goodLimit %d must exceed dropLimit %d", DefaultGoodLimit, DefaultDropLimit)
	}
	down := DefaultStep
	up := DefaultStep / 2
	if up >= down {
		t.Fatalf("recovery step %d must be smaller than the back-off step %d", up, down)
	}
}

func TestQualityRespectsBounds(t *testing.T) {
	t.Run("does not fall below min", func(t *testing.T) {
		q := NewQuality(40, 90, 45)
		for i := 0; i < DefaultDropLimit*20; i++ {
			q.OnDropped()
		}
		if q.Value() != 40 {
			t.Errorf("Value() = %d, want the floor 40", q.Value())
		}
	})

	t.Run("does not rise above max", func(t *testing.T) {
		q := NewQuality(40, 60, 58)
		for i := 0; i < DefaultGoodLimit*20; i++ {
			q.OnSent()
		}
		if q.Value() != 60 {
			t.Errorf("Value() = %d, want the ceiling 60", q.Value())
		}
	})
}

func TestQualitySetIsClampedAndClearsStreaks(t *testing.T) {
	q := NewQuality(30, 90, 50)

	q.Set(1000)
	if q.Value() != 90 {
		t.Errorf("Set(1000) gave %d, want the ceiling 90", q.Value())
	}
	q.Set(0)
	if q.Value() != 30 {
		t.Errorf("Set(0) gave %d, want the floor 30", q.Value())
	}

	// A manual choice replaces the value but not the bounds: a link that
	// cannot carry it must still be able to back off.
	q.Set(60)
	for i := 0; i < DefaultDropLimit; i++ {
		q.OnDropped()
	}
	if q.Value() != 60-DefaultStep {
		t.Errorf("Value() = %d after drops following Set(60), want %d", q.Value(), 60-DefaultStep)
	}
}

func TestQualitySetResetsStreaks(t *testing.T) {
	q := NewQuality(20, 90, 60)

	// Get most of the way to a back-off, then change the setting by hand.
	for i := 0; i < DefaultDropLimit-1; i++ {
		q.OnDropped()
	}
	q.Set(60)
	q.OnDropped()
	if q.Value() != 60 {
		t.Errorf("Value() = %d; Set should have cleared the drop streak", q.Value())
	}
}

func TestQualityStepsDownRepeatedly(t *testing.T) {
	q := NewQuality(20, 90, 90)
	want := []int{82, 74, 66, 58, 50}
	for i, w := range want {
		for j := 0; j < DefaultDropLimit; j++ {
			q.OnDropped()
		}
		if q.Value() != w {
			t.Fatalf("after back-off %d: Value() = %d, want %d", i+1, q.Value(), w)
		}
	}
}
