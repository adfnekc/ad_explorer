package rdp

// Scancodes for the browser's KeyboardEvent.code values.
//
// The browser reports the physical key in `code` rather than the character it
// produced in `key`, which is what the RDP keyboard channel wants: a French
// layout and a US layout press the same scancode for the same physical key, and
// the remote applies its own layout.
//
// grdp accepts a PC set 1 scancode, with one extension: a value in
// 0xE000..0xE0FF means the low byte is the scancode and 0xE0 is sent as the
// protocol's extended-key prefix (client/client.go, KeyUp/KeyDown).
const (
	// extension marks a key that the protocol encodes with an 0xE0 prefix.
	extension uint16 = 0xE000

	scEscape        = 0x01
	scBackspace     = 0x0E
	scTab           = 0x0F
	scEnter         = 0x1C
	scControlLeft   = 0x1D
	scShiftLeft     = 0x2A
	scShiftRight    = 0x36
	scAltLeft       = 0x38
	scSpace         = 0x39
	scCapsLock      = 0x3A
	scNumLock       = 0x45
	scScrollLock    = 0x46
	scNumpadMul     = 0x37
	scNumpadSub     = 0x4A
	scNumpadAdd     = 0x4E
	scNumpadDecimal = 0x53
	scIntlBackslash = 0x56
)

// scancodeByCode maps KeyboardEvent.code to a scancode. Multiples of 0xE000
// carry the extended prefix.
var scancodeByCode = map[string]uint16{
	"Escape": scEscape,

	// Digit row.
	"Digit1": 0x02, "Digit2": 0x03, "Digit3": 0x04, "Digit4": 0x05,
	"Digit5": 0x06, "Digit6": 0x07, "Digit7": 0x08, "Digit8": 0x09,
	"Digit9": 0x0A, "Digit0": 0x0B,
	"Minus": 0x0C, "Equal": 0x0D, "Backspace": scBackspace, "Tab": scTab,

	// Letter rows.
	"KeyQ": 0x10, "KeyW": 0x11, "KeyE": 0x12, "KeyR": 0x13, "KeyT": 0x14,
	"KeyY": 0x15, "KeyU": 0x16, "KeyI": 0x17, "KeyO": 0x18, "KeyP": 0x19,
	"BracketLeft": 0x1A, "BracketRight": 0x1B, "Enter": scEnter,
	"ControlLeft": scControlLeft,
	"KeyA":        0x1E, "KeyS": 0x1F, "KeyD": 0x20, "KeyF": 0x21, "KeyG": 0x22,
	"KeyH": 0x23, "KeyJ": 0x24, "KeyK": 0x25, "KeyL": 0x26,
	"Semicolon": 0x27, "Quote": 0x28, "Backquote": 0x29,
	"ShiftLeft": scShiftLeft, "Backslash": 0x2B,
	"KeyZ": 0x2C, "KeyX": 0x2D, "KeyC": 0x2E, "KeyV": 0x2F, "KeyB": 0x30,
	"KeyN": 0x31, "KeyM": 0x32,
	"Comma": 0x33, "Period": 0x34, "Slash": 0x35, "ShiftRight": scShiftRight,
	"NumpadMultiply": scNumpadMul,
	"AltLeft":        scAltLeft, "Space": scSpace, "CapsLock": scCapsLock,

	// Function keys.
	"F1": 0x3B, "F2": 0x3C, "F3": 0x3D, "F4": 0x3E, "F5": 0x3F,
	"F6": 0x40, "F7": 0x41, "F8": 0x42, "F9": 0x43, "F10": 0x44,
	"F11": 0x57, "F12": 0x58,

	"NumLock": scNumLock, "ScrollLock": scScrollLock,

	// Numeric keypad, non-extended half.
	"Numpad7": 0x47, "Numpad8": 0x48, "Numpad9": 0x49, "NumpadSubtract": scNumpadSub,
	"Numpad4": 0x4B, "Numpad5": 0x4C, "Numpad6": 0x4D, "NumpadAdd": scNumpadAdd,
	"Numpad1": 0x4F, "Numpad2": 0x50, "Numpad3": 0x51, "Numpad0": 0x52,
	"NumpadDecimal": scNumpadDecimal,

	// The key left of Z on ISO layouts. On an ANSI keyboard the browser never
	// reports it, so it costs nothing to map.
	"IntlBackslash": scIntlBackslash,

	// Extended keys: the protocol prefixes these with 0xE0, and the numeric
	// keypad keys below are the navigation keys when NumLock is off, which is
	// why they are distinct from the Numpad* entries above.
	"NumpadEnter":  extension | 0x1C,
	"ControlRight": extension | 0x1D,
	"NumpadDivide": extension | 0x35,
	"PrintScreen":  extension | 0x37,
	"AltRight":     extension | 0x38,
	"NumpadEqual":  extension | 0x59,
	"Home":         extension | 0x47,
	"ArrowUp":      extension | 0x48,
	"PageUp":       extension | 0x49,
	"ArrowLeft":    extension | 0x4B,
	"ArrowRight":   extension | 0x4D,
	"End":          extension | 0x4F,
	"ArrowDown":    extension | 0x50,
	"PageDown":     extension | 0x51,
	"Insert":       extension | 0x52,
	"Delete":       extension | 0x53,
	"MetaLeft":     extension | 0x5B,
	"MetaRight":    extension | 0x5C,
	"ContextMenu":  extension | 0x5D,
}

// ScancodeForCode resolves a KeyboardEvent.code to the scancode to hand to
// grdp.
//
// It reports false for keys this mapping does not cover, which includes Pause
// and the media keys: Pause needs the 0xE1 prefix, which grdp does not expose,
// and the media keys are not part of the PC set 1 keyboard.
func ScancodeForCode(code string) (uint16, bool) {
	sc, ok := scancodeByCode[code]
	return sc, ok
}

// IsExtended reports whether a scancode needs the protocol's 0xE0 prefix.
func IsExtended(sc uint16) bool { return sc&0xFF00 == extension }

// Scancodes for the sequences the gateway synthesises itself, because a browser
// cannot send them: Ctrl+Alt+Del is claimed by the operating system and never
// reaches the page.
const (
	ScEscape      = scEscape
	ScControlLeft = scControlLeft
	ScAltLeft     = scAltLeft
	ScDelete      = extension | 0x53
	ScEnter       = scEnter
)
