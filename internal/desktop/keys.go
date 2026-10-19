package desktop

import (
	"fmt"
	"strings"
)

// modifierAliases name the modifiers in agent key specs. "command" is the system key: ⌘ on macOS,
// Super on Linux, the Windows key on Windows.
var modifierAliases = map[string]string{
	"cmd": "command", "command": "command", "meta": "command", "super": "command", "win": "command",
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
