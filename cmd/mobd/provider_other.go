//go:build !linux

package main

import (
	"context"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/task"
)

// newProvider: macOS and Windows share the one real desktop (api.New's default).
func newProvider(config.Config, *task.Store) desktop.Provider { return nil }

// desktopsSupported: macOS and Windows have one shared desktop.
const desktopsSupported = false

func offlineList(config.Config) ([]desktop.Info, error) { return nil, nil }
func offlineClose(context.Context, config.Config, []string, desktop.CloseReason) error {
	return nil
}
func offlineDoctor(context.Context, config.Config) []desktop.DoctorCheck { return nil }
