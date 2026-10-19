package service

import (
	"fmt"
	"strings"
)

// renderUnit writes the systemd user unit. Logs go to mobd.log as on macOS: Debian keeps no
// persistent user journal by default, and mobd logs stays one tail on every OS.
func renderUnit(exe, cfg, workdir, logs, path string) []byte {
	return []byte(fmt.Sprintf(`[Unit]
Description=mobd office agent daemon
After=default.target

[Service]
Type=exec
ExecStart=%s serve --config %s
WorkingDirectory=%s
Environment=%s
StandardOutput=append:%s
StandardError=append:%s
Restart=on-failure
RestartSec=2
TimeoutStopSec=30
KillMode=mixed

[Install]
WantedBy=default.target
`, unitArg(exe), unitArg(cfg), unitPath(workdir), unitArg("PATH="+path), unitPath(logs), unitPath(logs)))
}

// unitArg quotes a command-line word or assignment for systemd: \ and " escaped, % and $ doubled.
func unitArg(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$", "\n", `\n`).Replace(s)
	return `"` + s + `"`
}

// unitPath is a path setting, where only specifiers (%) need escaping.
func unitPath(s string) string { return strings.ReplaceAll(s, "%", "%%") }
