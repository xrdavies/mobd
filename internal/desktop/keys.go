package desktop

import (
	"fmt"
	"strings"
)

// keyCodes are macOS virtual key codes (HIToolbox Events.h).
var keyCodes = map[string]int{
	"a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9,
	"b": 11, "q": 12, "w": 13, "e": 14, "r": 15, "y": 16, "t": 17,
	"1": 18, "2": 19, "3": 20, "4": 21, "6": 22, "5": 23, "=": 24, "9": 25, "7": 26,
	"-": 27, "8": 28, "0": 29, "]": 30, "o": 31, "u": 32, "[": 33, "i": 34, "p": 35,
	"l": 37, "j": 38, "'": 39, "k": 40, ";": 41, "\\": 42, ",": 43, "/": 44,
	"n": 45, "m": 46, ".": 47, "`": 50,
	"return": 36, "enter": 36, "tab": 48, "space": 49, "delete": 51, "backspace": 51,
	"escape": 53, "esc": 53, "command": 55, "cmd": 55, "shift": 56, "capslock": 57,
	"option": 58, "alt": 58, "control": 59, "ctrl": 59,
	"right": 124, "left": 123, "down": 125, "up": 126,
	"f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97,
	"f7": 98, "f8": 100, "f9": 101, "f10": 109, "f11": 103, "f12": 111,
}

var modifierAliases = map[string]string{
	"cmd": "command", "command": "command", "meta": "command",
	"ctrl": "control", "control": "control",
	"alt": "option", "option": "option",
	"shift": "shift",
}

// ParseHotkey splits "cmd+c" / "ctrl+shift+tab" into a key and modifiers.
func ParseHotkey(spec string) (string, []string, error) {
	var parts []string
	for p := range strings.SplitSeq(strings.ReplaceAll(spec, "-", "+"), "+") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "", nil, fmt.Errorf("empty key specification")
	}
	modifiers := []string{}
	key := ""
	for _, part := range parts {
		if m, ok := modifierAliases[part]; ok {
			modifiers = append(modifiers, m)
			continue
		}
		if key != "" {
			return "", nil, fmt.Errorf("multiple non-modifier keys in %q", spec)
		}
		key = part
	}
	if key == "" {
		return "", nil, fmt.Errorf("no key in specification %q", spec)
	}
	return key, modifiers, nil
}
