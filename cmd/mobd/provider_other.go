//go:build !linux

package main

import (
	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
)

// newProvider: macOS and Windows share the one real desktop (api.New's default).
func newProvider(config.Config) desktop.Provider { return nil }
