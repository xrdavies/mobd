//go:build linux

package x11

import (
	"context"
	"fmt"
	"os"
)

// OpenApp starts an application by desktop file ID, name or command, through the Launcher the
// desktop manager gave (it starts it in the desktop's apps cgroup).
func (b *Backend) OpenApp(ctx context.Context, target string) (string, error) {
	if b.launch == nil {
		return "", fmt.Errorf("opening applications is not available on this desktop")
	}
	a, err := findApp(target, appDirs(), os.Getenv("PATH"))
	if err != nil {
		return "", err
	}
	if _, err := b.launch(a.Argv); err != nil {
		return "", fmt.Errorf("open %s: %w", a.Name, err)
	}
	return fmt.Sprintf("opened %q (%s)", a.Name, a.From), nil
}
