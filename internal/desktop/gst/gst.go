package gst

import "time"

// Frame is one captured screen: BGRX pixels, Stride bytes a row.
type Frame struct {
	Pix                   []byte
	Stride, Width, Height int
	PTS                   time.Duration
}

// EncoderOptions are the encoded size (pixels, even), the frame rate bound and the bitrate.
type EncoderOptions struct{ Width, Height, FPS, Kbps int }

// Encoder turns frames into H.264 access units (Annex B, byte stream).
type Encoder interface {
	// Push queues one frame without blocking; a full queue drops the oldest.
	Push(f Frame)
	// Units delivers access units; it is closed after Close or an error (see Err).
	Units() <-chan Unit
	SetBitrate(kbps int)
	SetSize(width, height int) // the next unit is a keyframe at the new size
	ForceKeyframe()
	Err() error
	Close()
}

// Unit is one encoded access unit.
type Unit struct {
	Data []byte
	Key  bool
	PTS  time.Duration
}

// Factory makes one kind of encoder.
type Factory struct {
	Name   string // "vah264enc", "vaapih264enc", "nvh264enc", "openh264enc"
	Detail string // "VA-API", "NVENC", "software"
	New    func(EncoderOptions) (Encoder, error)
}
