package rdp

import "testing"

func TestScancodeForCodeKnownValues(t *testing.T) {
	// A spread across the table: the first key, the digit row, a letter, a
	// modifier, a function key and the numeric keypad.
	cases := []struct {
		code string
		want uint16
	}{
		{"Escape", 0x01},
		{"Digit1", 0x02},
		{"Digit0", 0x0B},
		{"Backspace", 0x0E},
		{"Tab", 0x0F},
		{"KeyQ", 0x10},
		{"Enter", 0x1C},
		{"ControlLeft", 0x1D},
		{"KeyA", 0x1E},
		{"ShiftLeft", 0x2A},
		{"AltLeft", 0x38},
		{"Space", 0x39},
		{"F1", 0x3B},
		{"F10", 0x44},
		{"F11", 0x57},
		{"F12", 0x58},
		{"Numpad0", 0x52},
		{"NumpadMultiply", 0x37},
	}

	for _, tc := range cases {
		got, ok := ScancodeForCode(tc.code)
		if !ok {
			t.Errorf("ScancodeForCode(%q) not found", tc.code)
			continue
		}
		if got != tc.want {
			t.Errorf("ScancodeForCode(%q) = %#04x, want %#04x", tc.code, got, tc.want)
		}
	}
}

func TestScancodeExtendedKeys(t *testing.T) {
	// These are the keys the protocol encodes with an 0xE0 prefix. Getting the
	// prefix wrong sends a different key entirely — an arrow key without it
	// arrives as a numeric keypad digit.
	cases := []struct {
		code string
		want uint16
	}{
		{"NumpadEnter", 0xE01C},
		{"ControlRight", 0xE01D},
		{"NumpadDivide", 0xE035},
		{"PrintScreen", 0xE037},
		{"AltRight", 0xE038},
		{"Home", 0xE047},
		{"ArrowUp", 0xE048},
		{"PageUp", 0xE049},
		{"ArrowLeft", 0xE04B},
		{"ArrowRight", 0xE04D},
		{"End", 0xE04F},
		{"ArrowDown", 0xE050},
		{"PageDown", 0xE051},
		{"Insert", 0xE052},
		{"Delete", 0xE053},
		{"MetaLeft", 0xE05B},
		{"ContextMenu", 0xE05D},
	}

	for _, tc := range cases {
		got, ok := ScancodeForCode(tc.code)
		if !ok {
			t.Errorf("ScancodeForCode(%q) not found", tc.code)
			continue
		}
		if got != tc.want {
			t.Errorf("ScancodeForCode(%q) = %#04x, want %#04x", tc.code, got, tc.want)
		}
		if !IsExtended(got) {
			t.Errorf("IsExtended(%#04x) = false for %q", got, tc.code)
		}
	}
}

func TestScancodeTableShape(t *testing.T) {
	// A guard against a truncated or mistyped table rather than a statement
	// about the layout: the mapping covers the whole PC set 1 keyboard.
	if len(scancodeByCode) < 100 {
		t.Errorf("table has %d entries; the PC set 1 keyboard needs more than 100", len(scancodeByCode))
	}

	seen := make(map[uint16]string, len(scancodeByCode))
	for code, sc := range scancodeByCode {
		lo := sc & 0x00FF
		if IsExtended(sc) {
			// The extension is the only value the high byte may take.
			if sc&0xFF00 != extension {
				t.Errorf("%s: %#04x has a high byte that is not the 0xE0 extension", code, sc)
			}
		} else {
			if sc > 0x58 {
				t.Errorf("%s: non-extended scancode %#04x is above 0x58", code, sc)
			}
			if lo == 0 {
				t.Errorf("%s: scancode is zero", code)
			}
		}
		if prev, dup := seen[sc]; dup {
			t.Errorf("%s and %s both map to %#04x", prev, code, sc)
		}
		seen[sc] = code
	}
}

func TestScancodeForUnknownCode(t *testing.T) {
	for _, code := range []string{"", "Unidentified", "Pause", "MediaPlayPause", "F13", "VolumeUp"} {
		if _, ok := ScancodeForCode(code); ok {
			t.Errorf("ScancodeForCode(%q) found a mapping; Pause needs a 0xE1 prefix grdp does not expose and the media keys are not PC set 1", code)
		}
	}
}

func TestIsExtended(t *testing.T) {
	if IsExtended(ScDelete) != true {
		t.Error("Delete must be extended")
	}
	if IsExtended(ScEnter) != false {
		t.Error("Enter must not be extended")
	}
	if IsExtended(0xE0FF) != true || IsExtended(0xE100) != false {
		t.Error("the extension test must accept exactly 0xE0xx")
	}
}

func TestSynthesisedSequenceConstants(t *testing.T) {
	// Ctrl+Alt+End is built from these; each must be the key it claims to be.
	if sc, _ := ScancodeForCode("ControlLeft"); ScControlLeft != sc {
		t.Errorf("ScControlLeft = %#04x, want the ControlLeft scancode %#04x", ScControlLeft, sc)
	}
	if sc, _ := ScancodeForCode("AltLeft"); ScAltLeft != sc {
		t.Errorf("ScAltLeft = %#04x, want the AltLeft scancode %#04x", ScAltLeft, sc)
	}
	if sc, _ := ScancodeForCode("Delete"); ScDelete != sc {
		t.Errorf("ScDelete = %#04x, want the Delete scancode %#04x", ScDelete, sc)
	}
	if sc, _ := ScancodeForCode("Enter"); ScEnter != sc {
		t.Errorf("ScEnter = %#04x, want the Enter scancode %#04x", ScEnter, sc)
	}
}
