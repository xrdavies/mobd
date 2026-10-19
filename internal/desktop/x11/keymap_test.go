package x11

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// mapKeys returns the quoted keys of the map literal that starts at marker in a Go file.
func mapKeys(t *testing.T, file, marker string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		t.Skip("run from the source tree (go test) to compare with the macOS tables")
	}
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("%s: no %q", file, marker)
	}
	block := s[i : i+strings.Index(s[i:], "\n}")]
	var keys []string
	for _, m := range regexp.MustCompile(`("(?:[^"\\]|\\.)+"):`).FindAllStringSubmatch(block, -1) {
		k, err := strconv.Unquote(m[1])
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	return keys
}

// Every key the macOS backend knows has a Linux entry, so the apps and agents need not care.
func TestKeyTablesCoverMacOS(t *testing.T) {
	codes := mapKeys(t, "../remote_darwin.go", "var webKeyCodes")
	for c := 'A'; c <= 'Z'; c++ {
		codes = append(codes, "Key"+string(c))
	}
	for d := '0'; d <= '9'; d++ {
		codes = append(codes, "Digit"+string(d))
	}
	for _, f := range []string{"F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12"} {
		codes = append(codes, f)
	}
	for _, c := range codes {
		if _, ok := webEvdev[c]; !ok {
			t.Errorf("web code %q has no evdev scancode", c)
		}
	}
	names := mapKeys(t, "../keys_darwin.go", "var keyCodes")
	if len(names) < 60 {
		t.Fatalf("read only %d key names", len(names))
	}
	for _, n := range names {
		if _, ok := keyNames[n]; !ok {
			t.Errorf("key name %q has no keysym", n)
		}
	}
	for _, m := range []string{"command", "control", "option", "shift"} {
		if modifierKeysyms[m] == 0 {
			t.Errorf("modifier %q", m)
		}
	}
	// Spot checks against input-event-codes.h and keysymdef.h.
	for code, want := range map[string]int{"KeyA": 30, "KeyQ": 16, "KeyZ": 44, "KeyM": 50, "Digit1": 2, "Digit0": 11, "F1": 59, "F10": 68, "F11": 87, "Enter": 28, "ArrowLeft": 105} {
		if webEvdev[code] != want {
			t.Errorf("%s = %d, want %d", code, webEvdev[code], want)
		}
	}
	if keyNames["f12"] != 0xffc9 || keyNames["return"] != 0xff0d || keyNames["a"] != 'a' {
		t.Error("keysym spot checks")
	}
}

func TestRuneKeysym(t *testing.T) {
	for r, want := range map[rune]uint32{'a': 'a', '~': '~', 'é': 0xe9, '→': 0x1002192, '中': 0x1004e2d, '\t': 0xff09, '\n': 0xff0d} {
		if got := runeKeysym(r); got != want {
			t.Errorf("%q: %#x, want %#x", r, got, want)
		}
	}
}
