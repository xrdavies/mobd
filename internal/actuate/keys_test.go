package actuate

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseHotkey(t *testing.T) {
	for _, tc := range []struct {
		spec, key string
		mods      []string
	}{
		{"return", "return", []string{}},
		{"tab", "tab", []string{}},
		{"cmd+c", "c", []string{"command"}},
		{"command+c", "c", []string{"command"}},
		{"ctrl+shift+tab", "tab", []string{"control", "shift"}},
		{"alt+f4", "f4", []string{"option"}},
		{"CMD+V", "v", []string{"command"}},
	} {
		key, mods, err := ParseHotkey(tc.spec)
		if err != nil || key != tc.key || !reflect.DeepEqual(mods, tc.mods) {
			t.Fatalf("%q: key=%q mods=%v err=%v", tc.spec, key, mods, err)
		}
	}
	if _, _, err := ParseHotkey(""); err == nil {
		t.Fatal("empty spec accepted")
	}
	if _, _, err := ParseHotkey("cmd+shift"); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Fatalf("modifiers only: %v", err)
	}
	if _, _, err := ParseHotkey("a+b"); err == nil {
		t.Fatal("two keys accepted")
	}
}
