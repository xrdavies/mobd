package runner

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// SplitMCPName turns "mcp__mob-mcp__click" (Claude-style) into ("mob-mcp", "click").
// Names without the prefix are returned as tool names with no server.
func SplitMCPName(name string) (server, tool string) {
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		if i := strings.Index(rest, "__"); i > 0 {
			return rest[:i], rest[i+2:]
		}
	}
	return "", name
}

// ToolTitle is the short human label for a desktop tool call, e.g. "Click (412, 230)".
func ToolTitle(tool string, raw json.RawMessage) string {
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	str := func(k string) string { s, _ := args[k].(string); return s }
	num := func(k string) string {
		if v, ok := args[k].(float64); ok {
			return fmt.Sprintf("%g", v)
		}
		return "?"
	}
	switch tool {
	case "screenshot":
		return "Screenshot"
	case "click":
		verb := "Click"
		if c, ok := args["count"].(float64); ok && c == 2 {
			verb = "Double-click"
		}
		if str("button") == "right" {
			verb = "Right-click"
		}
		return fmt.Sprintf("%s (%s, %s)", verb, num("x"), num("y"))
	case "type_text":
		return "Type “" + Clip(str("text"), 40) + "”"
	case "key":
		return "Press " + str("combo")
	case "open_app":
		return "Open " + str("name_or_bundle_id")
	case "list_windows":
		return "List windows"
	case "ask_user":
		return "Asked you: " + Clip(str("question"), 60)
	case "request_approval":
		return "Asked approval: " + Clip(str("action"), 60)
	}
	return strings.ReplaceAll(tool, "_", " ")
}

// CommandTitle labels a shell command, without the `zsh -lc '…'` wrapper.
func CommandTitle(cmd string) string {
	return "Run " + Clip(FirstLine(unwrapShell(cmd)), 60)
}

var shellWrapper = regexp.MustCompile(`^\S*\b(?:zsh|bash|sh) -l?c (?:'(.*)'|"(.*)")$`)

func unwrapShell(cmd string) string {
	if m := shellWrapper.FindStringSubmatch(strings.TrimSpace(cmd)); m != nil {
		return m[1] + m[2]
	}
	return cmd
}

// FirstLine keeps the first line, marking that more followed.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// Clip shortens s to n runes with an ellipsis.
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Plural returns "s" unless n is 1.
func Plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
