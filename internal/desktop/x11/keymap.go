package x11

import "fmt"

// webEvdev maps web KeyboardEvent.code names to Linux evdev scancodes (input-event-codes.h).
// Xvfb and setxkbmap use the "evdev" XKB rules, where the X keycode is the scancode + 8. Fn
// (0) has no scancode: the keyboard handles it, so it is accepted and ignored.
var webEvdev = func() map[string]int {
	m := map[string]int{
		"Escape": 1, "Minus": 12, "Equal": 13, "Backspace": 14, "Tab": 15,
		"BracketLeft": 26, "BracketRight": 27, "Enter": 28, "ControlLeft": 29,
		"Semicolon": 39, "Quote": 40, "Backquote": 41, "ShiftLeft": 42, "Backslash": 43,
		"Comma": 51, "Period": 52, "Slash": 53, "ShiftRight": 54, "NumpadMultiply": 55,
		"AltLeft": 56, "Space": 57, "CapsLock": 58, "NumLock": 69, "ScrollLock": 70,
		"Numpad7": 71, "Numpad8": 72, "Numpad9": 73, "NumpadSubtract": 74,
		"Numpad4": 75, "Numpad5": 76, "Numpad6": 77, "NumpadAdd": 78,
		"Numpad1": 79, "Numpad2": 80, "Numpad3": 81, "Numpad0": 82, "NumpadDecimal": 83,
		"IntlBackslash": 86, "F11": 87, "F12": 88, "IntlRo": 89,
		"NumpadEnter": 96, "ControlRight": 97, "NumpadDivide": 98, "PrintScreen": 99,
		"AltRight": 100, "Home": 102, "ArrowUp": 103, "PageUp": 104, "ArrowLeft": 105,
		"ArrowRight": 106, "End": 107, "ArrowDown": 108, "PageDown": 109, "Insert": 110,
		"Delete": 111, "NumpadEqual": 117, "Pause": 119, "IntlYen": 124,
		"MetaLeft": 125, "MetaRight": 126, "ContextMenu": 127, "Fn": 0,
	}
	for i, c := range "1234567890" {
		m["Digit"+string(c)] = 2 + i
	}
	for i, row := range []string{"QWERTYUIOP", "ASDFGHJKL", "ZXCVBNM"} {
		first := []int{16, 30, 44}[i]
		for j, c := range row {
			m["Key"+string(c)] = first + j
		}
	}
	for i := 1; i <= 10; i++ {
		m[fmt.Sprintf("F%d", i)] = 58 + i
	}
	return m
}()

// Keysyms (X11 keysymdef.h) of the keys agents name.
const (
	xkReturn    = 0xff0d
	xkTab       = 0xff09
	xkBackSpace = 0xff08
	xkEscape    = 0xff1b
	xkDelete    = 0xffff
	xkShiftL    = 0xffe1
	xkControlL  = 0xffe3
	xkAltL      = 0xffe9
	xkSuperL    = 0xffeb
)

// keyNames maps the key names agents use (desktop.ParseHotkey, the macOS table in keys_darwin.go)
// to keysyms. "delete" is Backspace, as the key with that label on a Mac keyboard.
var keyNames = func() map[string]uint32 {
	m := map[string]uint32{
		"return": xkReturn, "enter": xkReturn, "tab": xkTab, "space": ' ',
		"delete": xkBackSpace, "backspace": xkBackSpace, "forwarddelete": xkDelete,
		"escape": xkEscape, "esc": xkEscape, "capslock": 0xffe5,
		"command": xkSuperL, "cmd": xkSuperL, "shift": xkShiftL,
		"option": xkAltL, "alt": xkAltL, "control": xkControlL, "ctrl": xkControlL,
		"left": 0xff51, "up": 0xff52, "right": 0xff53, "down": 0xff54,
		"home": 0xff50, "end": 0xff57, "pageup": 0xff55, "pagedown": 0xff56, "insert": 0xff63,
	}
	for _, c := range "abcdefghijklmnopqrstuvwxyz0123456789=-][';\\,/.`" {
		m[string(c)] = uint32(c)
	}
	for i := 1; i <= 12; i++ {
		m[fmt.Sprintf("f%d", i)] = 0xffbe + uint32(i-1)
	}
	return m
}()

// modifierKeysyms are the keys held for the modifiers of desktop.ParseHotkey.
var modifierKeysyms = map[string]uint32{"command": xkSuperL, "control": xkControlL, "option": xkAltL, "shift": xkShiftL}

// runeKeysym is the keysym that types r: Latin-1 keysyms are the code point, the rest of
// Unicode is 0x01000000 + code point.
func runeKeysym(r rune) uint32 {
	switch {
	case r == '\t':
		return xkTab
	case r == '\n', r == '\r':
		return xkReturn
	case r >= 0x20 && r <= 0x7e, r >= 0xa0 && r <= 0xff:
		return uint32(r)
	}
	return 0x01000000 | uint32(r)
}
