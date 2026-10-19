// Package gst encodes captured frames as H.264 through GStreamer (cgo), with the hardware
// encoders the machine has and OpenH264 in software (docs/linux-design.md, section 2.7). It
// builds on Linux; without cgo Probe finds no encoders.
package gst
