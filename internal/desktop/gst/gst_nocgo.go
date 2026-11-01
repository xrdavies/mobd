//go:build linux && !cgo

package gst

import (
	"context"
	"errors"
)

// Probe without cgo finds no encoders: mobd built with CGO_ENABLED=0 has no live view.
func Probe(context.Context) []Factory { return nil }

// Available is empty without cgo.
func Available() []string { return nil }

// CanCaptureAudio is false without cgo: no desktop sound.
func CanCaptureAudio() bool { return false }

// CaptureAudio needs cgo.
func CaptureAudio(string, string) (Audio, error) {
	return nil, errors.New("mobd was built without cgo")
}
