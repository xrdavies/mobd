package runner

import (
	"encoding/json"
	"testing"
)

func TestActivityHelpers(t *testing.T) {
	for _, tc := range []struct{ name, server, tool string }{
		{"mcp__mob-mcp__click", "mob-mcp", "click"},
		{"mcp__a__b__c", "a", "b__c"},
		{"Bash", "", "Bash"},
		{"mcp__broken", "", "mcp__broken"},
	} {
		if s, tl := SplitMCPName(tc.name); s != tc.server || tl != tc.tool {
			t.Fatalf("%s → %q %q", tc.name, s, tl)
		}
	}
	if got := ToolTitle("click", json.RawMessage(`{"x":412,"y":230,"count":2}`)); got != "Double-click (412, 230)" {
		t.Fatal(got)
	}
	if got := ToolTitle("request_approval", json.RawMessage(`{"action":"Pay $420"}`)); got != "Asked approval: Pay $420" {
		t.Fatal(got)
	}
	if got := CommandTitle("/bin/zsh -lc 'echo hi'"); got != "Run echo hi" {
		t.Fatal(got)
	}
	if got := Clip("abcdef", 3); got != "abc…" {
		t.Fatal(got)
	}
}
