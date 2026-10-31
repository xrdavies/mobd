// Desktop audio capture for the macOS backend (audio_darwin.m): ScreenCaptureKit system audio,
// without Mob's and mobd's own playback, encoded to Opus (48 kHz, stereo, 20 ms) with
// AudioToolbox. Packets are delivered to Go through the exported mobAudioPacket.
#include <stdint.h>

// mob_audio_start returns a capture handle, or NULL with *rc set to a MOB_SNAP_* code
// (snapshot_darwin.h) and msg describing the failure. ident is passed back with every packet.
void *mob_audio_start(uintptr_t ident, double timeout_seconds, int *rc, char *msg, int msg_len);

// mob_audio_stop ends capture and frees the handle. No packets arrive after it.
void mob_audio_stop(void *audio);
