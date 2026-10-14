package actuate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Capture returns a PNG of the main display via screencapture(1).
func Capture(ctx context.Context) ([]byte, error) {
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
