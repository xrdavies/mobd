package codex

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaults reads the model and reasoning effort Codex uses when a turn names none: the
// top-level model and model_reasoning_effort of $CODEX_HOME/config.toml (~/.codex), overridden
// by the selected profile's. Only quoted string values are read; anything else is left out.
func defaults() (model, effort string) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		dir, err := os.UserHomeDir()
		if err != nil {
			return "", ""
		}
		home = filepath.Join(dir, ".codex")
	}
	f, err := os.Open(filepath.Join(home, "config.toml"))
	if err != nil {
		return "", ""
	}
	defer f.Close()
	tables := map[string]map[string]string{}
	table := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			table = strings.Trim(line, "[] ")
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if v, err := strconv.Unquote(strings.TrimSpace(stripComment(value))); err == nil {
			if tables[table] == nil {
				tables[table] = map[string]string{}
			}
			tables[table][strings.TrimSpace(key)] = v
		}
	}
	top := tables[""]
	model, effort = top["model"], top["model_reasoning_effort"]
	if p := tables["profiles."+top["profile"]]; top["profile"] != "" && p != nil {
		if p["model"] != "" {
			model = p["model"]
		}
		if p["model_reasoning_effort"] != "" {
			effort = p["model_reasoning_effort"]
		}
	}
	return model, effort
}

// stripComment drops a trailing # comment outside a quoted string.
func stripComment(v string) string {
	quoted := false
	for i, c := range v {
		switch {
		case c == '"' && (i == 0 || v[i-1] != '\\'):
			quoted = !quoted
		case c == '#' && !quoted:
			return v[:i]
		}
	}
	return v
}
