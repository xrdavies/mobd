//go:build linux && cgo

#include <gst/gst.h>
#include <gst/app/gstappsrc.h>
#include <gst/app/gstappsink.h>
#include <gst/video/video.h>
#include <string.h>
#include "gst_linux.h"

struct MobPipe {
	GstElement *pipe, *src, *sink;
};

int mob_gst_init(void) {
	GError *err = NULL;
	if (!gst_init_check(NULL, NULL, &err)) {
		if (err) g_error_free(err);
		return -1;
	}
	return 0;
}

int mob_gst_has(const char *factory) {
	GstElementFactory *f = gst_element_factory_find(factory);
	if (!f) return 0;
	gst_object_unref(f);
	return 1;
}

static char *dupmsg(const char *s) { return strdup(s ? s : "unknown GStreamer error"); }

MobPipe *mob_pipe_new(const char *desc, char **errout) {
	GError *err = NULL;
	GstElement *pipe = gst_parse_launch(desc, &err);
	if (err) {
		*errout = dupmsg(err->message);
		g_error_free(err);
		if (pipe) gst_object_unref(pipe);
		return NULL;
	}
	MobPipe *p = calloc(1, sizeof *p);
	p->pipe = pipe;
	p->src = gst_bin_get_by_name(GST_BIN(pipe), "src");
	p->sink = gst_bin_get_by_name(GST_BIN(pipe), "sink");
	// An encoder is fed through appsrc "src"; a capture (audio) has its own source element.
	if (!p->sink) {
		*errout = dupmsg("pipeline lacks appsink sink");
		mob_pipe_free(p);
		return NULL;
	}
	if (gst_element_set_state(pipe, GST_STATE_PLAYING) == GST_STATE_CHANGE_FAILURE) {
		char *e = mob_pipe_error(p);
		*errout = e ? e : dupmsg("pipeline did not start");
		mob_pipe_free(p);
		return NULL;
	}
	return p;
}

int mob_pipe_push(MobPipe *p, const void *data, int size, int64_t pts, const char *caps) {
	if (!p->src) return -1;
	if (caps) {
		GstCaps *c = gst_caps_from_string(caps);
		gst_app_src_set_caps(GST_APP_SRC(p->src), c);
		gst_caps_unref(c);
	}
	GstBuffer *buf = gst_buffer_new_allocate(NULL, size, NULL);
	gst_buffer_fill(buf, 0, data, size);
	GST_BUFFER_PTS(buf) = pts;
	GST_BUFFER_DTS(buf) = pts;
	return gst_app_src_push_buffer(GST_APP_SRC(p->src), buf) == GST_FLOW_OK ? 0 : -1;
}

int mob_pipe_pull(MobPipe *p, int64_t timeout, void **data, int *size, int *key) {
	GstSample *s = gst_app_sink_try_pull_sample(GST_APP_SINK(p->sink), timeout);
	if (!s) return gst_app_sink_is_eos(GST_APP_SINK(p->sink)) ? -1 : 0;
	GstBuffer *buf = gst_sample_get_buffer(s);
	GstMapInfo m;
	if (!buf || !gst_buffer_map(buf, &m, GST_MAP_READ)) {
		gst_sample_unref(s);
		return 0;
	}
	*data = malloc(m.size);
	memcpy(*data, m.data, m.size);
	*size = (int)m.size;
	*key = !GST_BUFFER_FLAG_IS_SET(buf, GST_BUFFER_FLAG_DELTA_UNIT);
	gst_buffer_unmap(buf, &m);
	gst_sample_unref(s);
	return 1;
}

char *mob_pipe_error(MobPipe *p) {
	GstBus *bus = gst_element_get_bus(p->pipe);
	GstMessage *msg = gst_bus_pop_filtered(bus, GST_MESSAGE_ERROR);
	gst_object_unref(bus);
	if (!msg) return NULL;
	GError *err = NULL;
	gst_message_parse_error(msg, &err, NULL);
	char *out = dupmsg(err ? err->message : NULL);
	if (err) g_error_free(err);
	gst_message_unref(msg);
	return out;
}

void mob_pipe_set_uint(MobPipe *p, const char *element, const char *prop, unsigned int value) {
	GstElement *e = gst_bin_get_by_name(GST_BIN(p->pipe), element);
	if (!e) return;
	GParamSpec *spec = g_object_class_find_property(G_OBJECT_GET_CLASS(e), prop);
	if (spec && spec->value_type == G_TYPE_UINT) g_object_set(e, prop, (guint)value, NULL);
	else if (spec && spec->value_type == G_TYPE_INT) g_object_set(e, prop, (gint)value, NULL);
	gst_object_unref(e);
}

void mob_pipe_set_caps(MobPipe *p, const char *element, const char *caps) {
	GstElement *e = gst_bin_get_by_name(GST_BIN(p->pipe), element);
	if (!e) return;
	GstCaps *c = gst_caps_from_string(caps);
	g_object_set(e, "caps", c, NULL);
	gst_caps_unref(c);
	gst_object_unref(e);
}

void mob_pipe_force_key(MobPipe *p) {
	// Upstream from the sink: h264parse passes it to the encoder.
	gst_element_send_event(p->sink, gst_video_event_new_upstream_force_key_unit(GST_CLOCK_TIME_NONE, TRUE, 0));
}

void mob_pipe_eos(MobPipe *p) {
	if (p->src) gst_app_src_end_of_stream(GST_APP_SRC(p->src));
	else gst_element_send_event(p->pipe, gst_event_new_eos());
}

void mob_pipe_free(MobPipe *p) {
	if (!p) return;
	if (p->pipe) gst_element_set_state(p->pipe, GST_STATE_NULL);
	if (p->src) gst_object_unref(p->src);
	if (p->sink) gst_object_unref(p->sink);
	if (p->pipe) gst_object_unref(p->pipe);
	free(p);
}

void mob_free(void *p) { free(p); }
