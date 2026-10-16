package desktop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// macOS drives the desktop with CoreGraphics events (input_darwin.go) and ScreenCaptureKit
// (snapshot_darwin.go).
type macOS struct{}

func newBackend() Backend { return macOS{} }

func (macOS) Name() string { return "macos" }

// screencapturePNG captures the main display with screencapture(1): the macOS 13 fallback for
// ScreenCaptureKit screenshots (snapshot_darwin.go).
func screencapturePNG(ctx context.Context) ([]byte, error) {
	f, err := os.CreateTemp("", "mob-screen-*.png")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)
	// -x: no shutter sound; -m: main display only.
	out, err := exec.CommandContext(ctx, "screencapture", "-x", "-m", "-t", "png", path).CombinedOutput()
	data, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || len(data) == 0 {
		return nil, fmt.Errorf("screencapture failed; grant Screen Recording to the app that launches mobd: %s %v",
			strings.TrimSpace(string(out)), err)
	}
	return data, nil
}

// Paste copies text to the pasteboard and presses cmd+v.
func (m macOS) Paste(ctx context.Context, text string) error {
	cmd := exec.CommandContext(ctx, "pbcopy")
	cmd.Env = append(cmd.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pbcopy failed: %v %s", err, strings.TrimSpace(string(out)))
	}
	return m.Key("v", []string{"command"})
}

// OpenApp tries `open -a` (name) and `open -b` (bundle ID), most likely first.
func (macOS) OpenApp(ctx context.Context, target string) (string, error) {
	attempts := [][]string{{"open", "-a", target}, {"open", "-b", target}}
	if looksLikeBundleID(target) {
		attempts[0], attempts[1] = attempts[1], attempts[0]
	}
	var failures []string
	for _, args := range attempts {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err == nil {
			return fmt.Sprintf("opened %q via %s", target, strings.Join(args, " ")), nil
		}
		failures = append(failures, strings.TrimSpace(string(out))+" "+err.Error())
	}
	return "", fmt.Errorf("open_app failed for %q: %s", target, strings.Join(failures, "; "))
}
