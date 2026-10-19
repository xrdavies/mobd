#include <stdint.h>

typedef struct MobPipe MobPipe;

// mob_gst_init initializes GStreamer once; it returns 0 when it works.
int mob_gst_init(void);
// mob_gst_has reports whether an element factory exists.
int mob_gst_has(const char *factory);
// mob_pipe_new builds and starts a pipeline with an appsrc "src" and an appsink "sink"; on
// failure it returns NULL and sets *err (free with mob_free).
MobPipe *mob_pipe_new(const char *desc, char **err);
// mob_pipe_push copies one buffer into the appsrc with a PTS in nanoseconds; caps may be NULL.
int mob_pipe_push(MobPipe *p, const void *data, int size, int64_t pts, const char *caps);
// mob_pipe_pull waits up to timeout ns for a sample: 1 with *data/*size (free with mob_free) and
// *key set, 0 on timeout, -1 at end of stream.
int mob_pipe_pull(MobPipe *p, int64_t timeout, void **data, int *size, int *key);
// mob_pipe_error returns the first error on the bus (free with mob_free), or NULL.
char *mob_pipe_error(MobPipe *p);
void mob_pipe_set_uint(MobPipe *p, const char *element, const char *prop, unsigned int value);
void mob_pipe_set_caps(MobPipe *p, const char *element, const char *caps);
void mob_pipe_force_key(MobPipe *p);
void mob_pipe_eos(MobPipe *p);
void mob_pipe_free(MobPipe *p);
void mob_free(void *p);
