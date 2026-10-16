// Live desktop stream: ScreenCaptureKit frames encoded with VideoToolbox. Manual reference counting
// (see snapshot_darwin.m). All stream state is touched only on the stream's serial queue, which is
// also where ScreenCaptureKit delivers frames, so adjustments from Go need no locks.
#import <CoreMedia/CoreMedia.h>
#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <VideoToolbox/VideoToolbox.h>
#include "snapshot_darwin.h"
#include "stream_darwin.h"

// Defined in Go (stream_darwin.go).
extern void mobStreamFrame(uintptr_t ident, void *data, int len, int key, int width, int height, long long pts_us);
extern void mobStreamEnded(uintptr_t ident, int rc, char *msg);

// Passed to the encoder's output callback; one per encoder session, since the size is per session.
typedef struct {
	uintptr_t ident;
	int width, height;
} MobSessionInfo;

static void mob_stream_message(char *msg, int len, NSString *s) {
	if (len > 0) snprintf(msg, len, "%s", s.UTF8String ?: "");
}

static int mob_stream_classify(NSError *e, char *msg, int len) {
	mob_stream_message(msg, len, e.localizedDescription);
	if (![e.domain isEqualToString:SCStreamErrorDomain]) return MOB_SNAP_FAILED;
	switch (e.code) {
	case -3801: return MOB_SNAP_DENIED;
	case -3813: case -3814: case -3815: return MOB_SNAP_NO_DISPLAY;
	default: return MOB_SNAP_FAILED;
	}
}

static const uint8_t mob_start_code[] = {0, 0, 0, 1};

// AVCC sample (length-prefixed NAL units) to Annex B with an AUD; keyframes carry SPS and PPS.
static void mob_vt_output(void *refcon, void *frame_refcon, OSStatus status, VTEncodeInfoFlags flags, CMSampleBufferRef sb) {
	if (status != noErr || sb == NULL || (flags & kVTEncodeInfo_FrameDropped)) return;
	MobSessionInfo *info = refcon;
	CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sb);
	if (block == NULL) return;
	@autoreleasepool {
		CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, false);
		BOOL key = YES;
		if (atts && CFArrayGetCount(atts) > 0) {
			CFBooleanRef notSync = CFDictionaryGetValue(CFArrayGetValueAtIndex(atts, 0), kCMSampleAttachmentKey_NotSync);
			key = !(notSync && CFBooleanGetValue(notSync));
		}
		size_t length = CMBlockBufferGetDataLength(block);
		NSMutableData *au = [NSMutableData dataWithCapacity:length + 64];
		const uint8_t aud[] = {0, 0, 0, 1, 0x09, 0xF0};
		[au appendBytes:aud length:sizeof aud];
		CMFormatDescriptionRef fd = CMSampleBufferGetFormatDescription(sb);
		if (key && fd) {
			size_t count = 0;
			CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, 0, NULL, NULL, &count, NULL);
			for (size_t i = 0; i < count; i++) {
				const uint8_t *p = NULL;
				size_t n = 0;
				if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, i, &p, &n, NULL, NULL) == noErr) {
					[au appendBytes:mob_start_code length:4];
					[au appendBytes:p length:n];
				}
			}
		}
		NSMutableData *raw = [NSMutableData dataWithLength:length];
		if (CMBlockBufferCopyDataBytes(block, 0, length, raw.mutableBytes) != kCMBlockBufferNoErr) return;
		const uint8_t *b = raw.bytes;
		size_t i = 0;
		while (i + 4 <= length) {
			size_t n = (size_t)b[i] << 24 | (size_t)b[i + 1] << 16 | (size_t)b[i + 2] << 8 | b[i + 3];
			i += 4;
			if (i + n > length) break;
			[au appendBytes:mob_start_code length:4];
			[au appendBytes:b + i length:n];
			i += n;
		}
		CMTime pts = CMSampleBufferGetPresentationTimeStamp(sb);
		long long us = CMTIME_IS_VALID(pts) ? (long long)(CMTimeGetSeconds(pts) * 1e6) : 0;
		mobStreamFrame(info->ident, au.mutableBytes, (int)au.length, key, info->width, info->height, us);
	}
}

@interface MobStream : NSObject <SCStreamOutput, SCStreamDelegate> {
@public
	uintptr_t ident;
	dispatch_queue_t queue;
	SCStream *stream;
	SCStreamConfiguration *config;
	VTCompressionSessionRef session;
	MobSessionInfo *info;
	CVPixelBufferRef last;
	int width, height, fps, kbps;
	BOOL forceKey, stopped;
}
@end

@implementation MobStream

- (void)setBitrate {
	if (session == NULL) return;
	VTSessionSetProperty(session, kVTCompressionPropertyKey_AverageBitRate, (CFNumberRef)@(kbps * 1000));
	// A hard cap of 1.5x the average per second, so a keyframe cannot blow far past the budget.
	NSArray *limit = @[@(kbps * 1000 / 8 * 3 / 2), @1];
	VTSessionSetProperty(session, kVTCompressionPropertyKey_DataRateLimits, (CFArrayRef)limit);
}

- (void)endSession {
	if (session) {
		VTCompressionSessionCompleteFrames(session, kCMTimeInvalid);
		VTCompressionSessionInvalidate(session);
		CFRelease(session);
		session = NULL;
	}
	free(info);
	info = NULL;
}

- (BOOL)makeSession {
	[self endSession];
	info = calloc(1, sizeof *info);
	info->ident = ident;
	info->width = width;
	info->height = height;
	NSDictionary *spec = @{(NSString *)kVTVideoEncoderSpecification_EnableLowLatencyRateControl: @YES};
	OSStatus st = VTCompressionSessionCreate(NULL, width, height, kCMVideoCodecType_H264, (CFDictionaryRef)spec,
		NULL, NULL, mob_vt_output, info, &session);
	if (st != noErr) {
		session = NULL;
		return NO;
	}
	VTSessionSetProperty(session, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue);
	VTSessionSetProperty(session, kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel);
	VTSessionSetProperty(session, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse);
	VTSessionSetProperty(session, kVTCompressionPropertyKey_ExpectedFrameRate, (CFNumberRef)@(fps));
	// No periodic keyframes: an idle screen sends nothing, and receivers ask for one when needed.
	VTSessionSetProperty(session, kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, (CFNumberRef)@3600);
	[self setBitrate];
	VTCompressionSessionPrepareToEncodeFrames(session);
	forceKey = YES;
	return YES;
}

- (void)encode:(CVPixelBufferRef)pb pts:(CMTime)pts {
	// After a resize, frames of the old size can still arrive; skip them.
	if (session == NULL || (int)CVPixelBufferGetWidth(pb) != width || (int)CVPixelBufferGetHeight(pb) != height) return;
	NSDictionary *props = forceKey ? @{(NSString *)kVTEncodeFrameOptionKey_ForceKeyFrame: @YES} : nil;
	forceKey = NO;
	VTCompressionSessionEncodeFrame(session, pb, pts, kCMTimeInvalid, (CFDictionaryRef)props, NULL, NULL);
}

- (void)stream:(SCStream *)s didOutputSampleBuffer:(CMSampleBufferRef)sb ofType:(SCStreamOutputType)type {
	if (type != SCStreamOutputTypeScreen || stopped || !CMSampleBufferIsValid(sb)) return;
	CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, false);
	if (atts == NULL || CFArrayGetCount(atts) == 0) return;
	NSDictionary *a = (NSDictionary *)CFArrayGetValueAtIndex(atts, 0);
	NSNumber *status = a[SCStreamFrameInfoStatus];
	// Idle, blank and suspended frames carry no new picture; nothing is encoded for them.
	if (status == nil || status.integerValue != SCFrameStatusComplete) return;
	CVPixelBufferRef pb = CMSampleBufferGetImageBuffer(sb);
	if (pb == NULL) return;
	if (last) CVPixelBufferRelease(last);
	last = CVPixelBufferRetain(pb);
	[self encode:pb pts:CMSampleBufferGetPresentationTimeStamp(sb)];
}

- (void)stream:(SCStream *)s didStopWithError:(NSError *)error {
	char buf[256] = {0};
	int rc = mob_stream_classify(error, buf, sizeof buf);
	NSString *msg = [[NSString alloc] initWithUTF8String:buf];
	dispatch_async(queue, ^{
		if (!stopped) {
			stopped = YES;
			mobStreamEnded(ident, rc, (char *)msg.UTF8String);
		}
		[msg release];
	});
}

- (void)dealloc {
	[self endSession];
	if (last) CVPixelBufferRelease(last);
	[stream release];
	[config release];
	if (queue) dispatch_release(queue);
	[super dealloc];
}
@end

void *mob_stream_start(uintptr_t ident, int width, int height, int fps, int kbps, int show_cursor,
	double timeout_seconds, int *rc, char *msg, int msg_len) {
	*rc = MOB_SNAP_OK;
	MobStream *s = nil;
	@autoreleasepool {
		dispatch_time_t deadline = dispatch_time(DISPATCH_TIME_NOW, (int64_t)(timeout_seconds * NSEC_PER_SEC));
		// __block storage moves to the heap with the block, so late completions stay safe after a timeout.
		__block SCDisplay *display = nil;
		__block NSError *err = nil;
		dispatch_semaphore_t done = dispatch_semaphore_create(0);
		[SCShareableContent getShareableContentExcludingDesktopWindows:NO onScreenWindowsOnly:YES
			completionHandler:^(SCShareableContent *content, NSError *error) {
				if (error) err = [error retain];
				for (SCDisplay *d in content.displays) {
					if (d.displayID == CGMainDisplayID()) display = [d retain];
				}
				dispatch_semaphore_signal(done);
			}];
		if (dispatch_semaphore_wait(done, deadline) != 0) {
			*rc = MOB_SNAP_TIMEOUT;
			mob_stream_message(msg, msg_len, @"screen capture timed out");
		} else if (err) {
			*rc = mob_stream_classify(err, msg, msg_len);
		} else if (display == nil) {
			*rc = MOB_SNAP_NO_DISPLAY;
			mob_stream_message(msg, msg_len, @"the main display is not capturable");
		}
		dispatch_release(done);
		if (*rc == MOB_SNAP_OK) {
			s = [[MobStream alloc] init];
			s->ident = ident;
			s->queue = dispatch_queue_create("mobd.stream", DISPATCH_QUEUE_SERIAL);
			s->width = width;
			s->height = height;
			s->fps = fps;
			s->kbps = kbps;
			if (![s makeSession]) {
				*rc = MOB_SNAP_FAILED;
				mob_stream_message(msg, msg_len, @"could not create the H.264 encoder");
			}
		}
		if (*rc == MOB_SNAP_OK) {
			SCStreamConfiguration *c = [[SCStreamConfiguration alloc] init];
			c.width = width;
			c.height = height;
			c.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange;
			c.minimumFrameInterval = CMTimeMake(1, fps);
			c.showsCursor = show_cursor != 0;
			c.queueDepth = 6;
			s->config = c;
			SCContentFilter *filter = [[[SCContentFilter alloc] initWithDisplay:display excludingWindows:@[]] autorelease];
			s->stream = [[SCStream alloc] initWithFilter:filter configuration:c delegate:s];
			NSError *addErr = nil;
			if (![s->stream addStreamOutput:s type:SCStreamOutputTypeScreen sampleHandlerQueue:s->queue error:&addErr]) {
				*rc = mob_stream_classify(addErr, msg, msg_len);
			}
		}
		if (*rc == MOB_SNAP_OK) {
			__block NSError *startErr = nil;
			dispatch_semaphore_t started = dispatch_semaphore_create(0);
			[s->stream startCaptureWithCompletionHandler:^(NSError *error) {
				if (error) startErr = [error retain];
				dispatch_semaphore_signal(started);
			}];
			if (dispatch_semaphore_wait(started, deadline) != 0) {
				*rc = MOB_SNAP_TIMEOUT;
				mob_stream_message(msg, msg_len, @"starting screen capture timed out");
			} else if (startErr) {
				*rc = mob_stream_classify(startErr, msg, msg_len);
			}
			dispatch_release(started);
		}
		[display release];
		[err release];
		if (*rc != MOB_SNAP_OK && s != nil) {
			s->stopped = YES;
			[s release];
			s = nil;
		}
	}
	return s;
}

void mob_stream_bitrate(void *p, int kbps) {
	MobStream *s = p;
	dispatch_async(s->queue, ^{
		s->kbps = kbps;
		[s setBitrate];
	});
}

void mob_stream_size(void *p, int width, int height) {
	MobStream *s = p;
	dispatch_async(s->queue, ^{
		if (s->stopped || (width == s->width && height == s->height)) return;
		s->width = width;
		s->height = height;
		if (s->last) CVPixelBufferRelease(s->last);
		s->last = NULL;
		[s makeSession];
		s->config.width = width;
		s->config.height = height;
		[s->stream updateConfiguration:s->config completionHandler:nil];
	});
}

void mob_stream_fps(void *p, int fps) {
	MobStream *s = p;
	dispatch_async(s->queue, ^{
		if (s->stopped || fps == s->fps) return;
		s->fps = fps;
		if (s->session) VTSessionSetProperty(s->session, kVTCompressionPropertyKey_ExpectedFrameRate, (CFNumberRef)@(fps));
		s->config.minimumFrameInterval = CMTimeMake(1, fps);
		[s->stream updateConfiguration:s->config completionHandler:nil];
	});
}

void mob_stream_keyframe(void *p) {
	MobStream *s = p;
	dispatch_async(s->queue, ^{
		if (s->stopped) return;
		s->forceKey = YES;
		// An idle screen delivers no frames, so encode the last picture again as a keyframe now.
		if (s->last) [s encode:s->last pts:CMClockGetTime(CMClockGetHostTimeClock())];
	});
}

void mob_stream_stop(void *p) {
	MobStream *s = p;
	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	[s->stream stopCaptureWithCompletionHandler:^(NSError *error) { dispatch_semaphore_signal(done); }];
	dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC));
	dispatch_release(done);
	// Runs after every queued adjustment; the encoder is flushed before the stream is freed.
	dispatch_sync(s->queue, ^{
		s->stopped = YES;
		[s endSession];
	});
	[s release];
}
