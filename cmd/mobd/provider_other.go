//go:build !linux

package main

import (
	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/task"
)

// newProvider: macOS and Windows share the one real desktop (api.New's default).
func newProvider(config.Config, *task.Store) desktop.Provider { return nil }
