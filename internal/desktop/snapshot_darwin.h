// Native screen snapshots for the macOS backend (snapshot_darwin.m).

enum {
	MOB_SNAP_OK = 0,
	MOB_SNAP_UNAVAILABLE = 1, // ScreenCaptureKit screenshots need macOS 14
	MOB_SNAP_DENIED = 2,      // Screen Recording not granted
	MOB_SNAP_NO_DISPLAY = 3,  // no capturable display: locked console or display asleep
	MOB_SNAP_TIMEOUT = 4,
	MOB_SNAP_FAILED = 5,
};

// mob_snapshot captures the main display without the cursor, scaled to at most max_width pixels
// (0 = native pixels), and encodes it as JPEG (quality 1-100) or PNG (quality 0). On success *out
// is malloc'd and owned by the caller. msg receives a description of other failures.
int mob_snapshot(int max_width, int quality, double timeout_seconds,
	void **out, long *out_len, int *out_width, int *out_height, char *msg, int msg_len);

// mob_display_state reports whether the console session is locked and the main display asleep.
void mob_display_state(int *locked, int *asleep);
