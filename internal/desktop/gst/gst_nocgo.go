//go:build linux && !cgo

package gst

import "context"

// Probe without cgo finds no encoders: mobd built with CGO_ENABLED=0 has no live view.
func Probe(context.Context) []Factory { return nil }

// Available is empty without cgo.
func Available() []string { return nil }
