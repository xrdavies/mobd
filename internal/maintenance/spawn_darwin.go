package maintenance

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

const helperLabel = "com.xrdavies.mobd.maintain"

// Start bootstraps a one-shot LaunchAgent (no KeepAlive) in the user's GUI domain.
func (h Helper) Start(id string) error {
	escape := func(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	var prog bytes.Buffer
	for _, a := range h.args(id) {
		fmt.Fprintf(&prog, "<string>%s</string>", escape(a))
	}
	logs := filepath.Join(h.Dir, "mobd.log")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array>%s</array>
<key>WorkingDirectory</key><string>%s</string>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><false/>
<key>AbandonProcessGroup</key><true/>
</dict></plist>
`, helperLabel, prog.String(), escape(h.Cwd), escape(h.Path), escape(logs), escape(logs))
	path := filepath.Join(h.Dir, "maintain.plist")
	if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	// A finished helper from an earlier operation stays loaded; replace it.
	_ = exec.Command("/bin/launchctl", "bootout", domain+"/"+helperLabel).Run()
	if out, err := exec.Command("/bin/launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
