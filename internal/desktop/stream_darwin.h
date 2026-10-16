// Live capture + H.264 encode for the macOS backend (stream_darwin.m). One stream owns an SCStream
// and a VideoToolbox session; captured frames never leave native code. Encoded access units (Annex B,
// AUD first, SPS/PPS on keyframes) are delivered to Go through the exported mobStreamFrame.
#include <stdint.h>

// mob_stream_start returns a stream handle, or NULL with *rc set to a MOB_SNAP_* code
// (snapshot_darwin.h) and msg describing the failure. ident is passed back with every frame.
void *mob_stream_start(uintptr_t ident, int width, int height, int fps, int kbps, int show_cursor,
	double timeout_seconds, int *rc, char *msg, int msg_len);

// The adjustments run asynchronously on the stream's queue; the stream is never restarted.
void mob_stream_bitrate(void *stream, int kbps);
void mob_stream_size(void *stream, int width, int height);
void mob_stream_keyframe(void *stream);
void mob_stream_fps(void *stream, int fps);

// mob_stream_stop ends capture, flushes the encoder and frees the stream. No frames arrive after it.
void mob_stream_stop(void *stream);
