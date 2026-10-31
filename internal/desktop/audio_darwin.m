// Desktop audio: ScreenCaptureKit captures the system's playback next to a tiny screen stream
// that is discarded (SCStream needs a display). Audio needs macOS 13 and the Screen Recording
// permission mobd already has. Manual reference counting, like stream_darwin.m; all state is
// touched on the capture's serial queue.
#import <AudioToolbox/AudioToolbox.h>
#import <CoreMedia/CoreMedia.h>
#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#include "snapshot_darwin.h"
#include "audio_darwin.h"

// Defined in Go (audio_darwin.go).
extern void mobAudioPacket(uintptr_t ident, void *data, int len, long long pts_us);
extern void mobAudioEnded(uintptr_t ident, int rc, char *msg);

enum { MOB_RATE = 48000, MOB_CH = 2, MOB_FRAME = 960, MOB_MAX_PENDING = MOB_RATE / 5 };

// The encoder's input callback ran out of samples; FillComplexBuffer returns it, keeping state.
static const OSStatus mob_no_data = 'nodt';

static void mob_audio_message(char *msg, int len, NSString *s) {
	if (len > 0) snprintf(msg, len, "%s", s.UTF8String ?: "");
}

static int mob_audio_classify(NSError *e, char *msg, int len) {
	mob_audio_message(msg, len, e.localizedDescription);
	if (![e.domain isEqualToString:SCStreamErrorDomain]) return MOB_SNAP_FAILED;
	switch (e.code) {
	case -3801: return MOB_SNAP_DENIED;
	case -3813: case -3814: case -3815: return MOB_SNAP_NO_DISPLAY;
	default: return MOB_SNAP_FAILED;
	}
}

@interface MobAudio : NSObject <SCStreamOutput, SCStreamDelegate> {
@public
	uintptr_t ident;
	dispatch_queue_t queue;
	SCStream *stream;
	AudioConverterRef encoder;
	float *pending;      // interleaved stereo samples not yet encoded
	int pendingFrames;   // frames in pending
	int fedFrames;       // frames of pending handed to the encoder
	long long pendingPts; // capture time of pending[0], microseconds
	BOOL stopped;
}
@end

static OSStatus mob_audio_feed(AudioConverterRef c, UInt32 *n, AudioBufferList *bl, AudioStreamPacketDescription **pd, void *u) {
	MobAudio *a = u;
	int left = a->pendingFrames - a->fedFrames;
	if (left <= 0) {
		*n = 0;
		return mob_no_data;
	}
	if (*n > (UInt32)left) *n = left;
	bl->mBuffers[0].mData = a->pending + a->fedFrames * MOB_CH;
	bl->mBuffers[0].mDataByteSize = *n * MOB_CH * sizeof(float);
	bl->mBuffers[0].mNumberChannels = MOB_CH;
	a->fedFrames += *n;
	return noErr;
}

@implementation MobAudio

- (BOOL)makeEncoder {
	AudioStreamBasicDescription in = {0};
	in.mSampleRate = MOB_RATE;
	in.mFormatID = kAudioFormatLinearPCM;
	in.mFormatFlags = kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked;
	in.mBytesPerPacket = in.mBytesPerFrame = MOB_CH * sizeof(float);
	in.mFramesPerPacket = 1;
	in.mChannelsPerFrame = MOB_CH;
	in.mBitsPerChannel = 32;
	AudioStreamBasicDescription out = {0};
	out.mSampleRate = MOB_RATE;
	out.mFormatID = kAudioFormatOpus;
	out.mChannelsPerFrame = MOB_CH;
	out.mFramesPerPacket = MOB_FRAME;
	if (AudioConverterNew(&in, &out, &encoder) != noErr) {
		encoder = NULL;
		return NO;
	}
	UInt32 bitrate = 96000;
	AudioConverterSetProperty(encoder, kAudioConverterEncodeBitRate, sizeof bitrate, &bitrate);
	return YES;
}

// append adds a sample buffer's audio, as interleaved float stereo, to pending.
- (void)append:(CMSampleBufferRef)sb {
	CMFormatDescriptionRef fd = CMSampleBufferGetFormatDescription(sb);
	const AudioStreamBasicDescription *asbd = fd ? CMAudioFormatDescriptionGetStreamBasicDescription(fd) : NULL;
	if (asbd == NULL || asbd->mFormatID != kAudioFormatLinearPCM || !(asbd->mFormatFlags & kAudioFormatFlagIsFloat) ||
		asbd->mBitsPerChannel != 32 || (int)asbd->mSampleRate != MOB_RATE || asbd->mChannelsPerFrame < 1) return;
	int frames = (int)CMSampleBufferGetNumSamples(sb);
	if (frames <= 0) return;
	size_t size = 0;
	CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(sb, &size, NULL, 0, NULL, NULL, 0, NULL);
	AudioBufferList *bl = malloc(size);
	CMBlockBufferRef block = NULL;
	if (CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(sb, NULL, bl, size, NULL, NULL,
			kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment, &block) != noErr) {
		free(bl);
		return;
	}
	// Too far behind (the encoder or Go stalled): drop the old samples rather than play them late.
	if (pendingFrames + frames > MOB_MAX_PENDING) {
		pendingFrames = fedFrames = 0;
	}
	if (pendingFrames == 0) {
		CMTime pts = CMSampleBufferGetPresentationTimeStamp(sb);
		pendingPts = CMTIME_IS_VALID(pts) ? (long long)(CMTimeGetSeconds(pts) * 1e6) : 0;
	}
	float *dst = pending + pendingFrames * MOB_CH;
	BOOL interleaved = !(asbd->mFormatFlags & kAudioFormatFlagIsNonInterleaved);
	int channels = (int)asbd->mChannelsPerFrame;
	for (int i = 0; i < frames; i++) {
		for (int c = 0; c < MOB_CH; c++) {
			int src = c < channels ? c : 0; // mono plays on both sides
			float v = 0;
			if (interleaved) {
				v = ((float *)bl->mBuffers[0].mData)[i * channels + src];
			} else if ((UInt32)src < bl->mNumberBuffers) {
				v = ((float *)bl->mBuffers[src].mData)[i];
			}
			dst[i * MOB_CH + c] = v;
		}
	}
	pendingFrames += frames;
	if (block) CFRelease(block);
	free(bl);
}

// drain encodes every whole 20 ms packet in pending.
- (void)drain {
	unsigned char out[1500];
	while (pendingFrames - fedFrames >= MOB_FRAME) {
		UInt32 n = 1;
		AudioStreamPacketDescription d = {0};
		AudioBufferList bl = {1, {{MOB_CH, sizeof out, out}}};
		int before = fedFrames;
		OSStatus st = AudioConverterFillComplexBuffer(encoder, mob_audio_feed, self, &n, &bl, &d);
		if (n == 1 && d.mDataByteSize > 0) {
			long long pts = pendingPts + (long long)before * 1000000 / MOB_RATE;
			mobAudioPacket(ident, out, (int)d.mDataByteSize, pts);
		}
		if (st != noErr && st != mob_no_data) break;
		if (n == 0 && fedFrames == before) break;
	}
	// Keep what the encoder has not taken.
	if (fedFrames > 0) {
		int left = pendingFrames - fedFrames;
		memmove(pending, pending + fedFrames * MOB_CH, left * MOB_CH * sizeof(float));
		pendingPts += (long long)fedFrames * 1000000 / MOB_RATE;
		pendingFrames = left;
		fedFrames = 0;
	}
}

- (void)stream:(SCStream *)s didOutputSampleBuffer:(CMSampleBufferRef)sb ofType:(SCStreamOutputType)type {
	if (type != SCStreamOutputTypeAudio || stopped || encoder == NULL || !CMSampleBufferIsValid(sb)) return;
	[self append:sb];
	[self drain];
}

- (void)stream:(SCStream *)s didStopWithError:(NSError *)error {
	char buf[256] = {0};
	int rc = mob_audio_classify(error, buf, sizeof buf);
	NSString *msg = [[NSString alloc] initWithUTF8String:buf];
	dispatch_async(queue, ^{
		if (!stopped) {
			stopped = YES;
			mobAudioEnded(ident, rc, (char *)msg.UTF8String);
		}
		[msg release];
	});
}

- (void)dealloc {
	if (encoder) AudioConverterDispose(encoder);
	free(pending);
	[stream release];
	if (queue) dispatch_release(queue);
	[super dealloc];
}
@end

// The screen output: frames of the tiny screen stream are ignored, but ScreenCaptureKit logs every
// frame dropped for want of an output.
@interface MobAudioScreen : NSObject <SCStreamOutput>
@end
@implementation MobAudioScreen
- (void)stream:(SCStream *)s didOutputSampleBuffer:(CMSampleBufferRef)sb ofType:(SCStreamOutputType)type {}
@end

static MobAudioScreen *mob_audio_screen;

void *mob_audio_start(uintptr_t ident, double timeout_seconds, int *rc, char *msg, int msg_len) {
	*rc = MOB_SNAP_OK;
	if (@available(macOS 13.0, *)) {
	} else {
		*rc = MOB_SNAP_FAILED;
		mob_audio_message(msg, msg_len, @"desktop sound needs macOS 13 or later");
		return NULL;
	}
	MobAudio *a = nil;
	@autoreleasepool {
		dispatch_time_t deadline = dispatch_time(DISPATCH_TIME_NOW, (int64_t)(timeout_seconds * NSEC_PER_SEC));
		__block SCDisplay *display = nil;
		__block NSArray<SCRunningApplication *> *mob = nil;
		__block NSError *err = nil;
		dispatch_semaphore_t done = dispatch_semaphore_create(0);
		[SCShareableContent getShareableContentExcludingDesktopWindows:NO onScreenWindowsOnly:YES
			completionHandler:^(SCShareableContent *content, NSError *error) {
				if (error) err = [error retain];
				for (SCDisplay *d in content.displays) {
					if (d.displayID == CGMainDisplayID()) display = [d retain];
				}
				// Mob Desktop on this computer: its playback of this very sound would echo.
				NSMutableArray *apps = [NSMutableArray array];
				for (SCRunningApplication *app in content.applications) {
					if ([app.bundleIdentifier isEqualToString:@"com.xrdavies.mob"]) [apps addObject:app];
				}
				mob = [apps retain];
				dispatch_semaphore_signal(done);
			}];
		if (dispatch_semaphore_wait(done, deadline) != 0) {
			*rc = MOB_SNAP_TIMEOUT;
			mob_audio_message(msg, msg_len, @"screen capture timed out");
		} else if (err) {
			*rc = mob_audio_classify(err, msg, msg_len);
		} else if (display == nil) {
			*rc = MOB_SNAP_NO_DISPLAY;
			mob_audio_message(msg, msg_len, @"the main display is not capturable");
		}
		dispatch_release(done);
		if (*rc == MOB_SNAP_OK) {
			a = [[MobAudio alloc] init];
			a->ident = ident;
			a->queue = dispatch_queue_create("mobd.audio", DISPATCH_QUEUE_SERIAL);
			a->pending = calloc(MOB_MAX_PENDING * MOB_CH, sizeof(float));
			if (![a makeEncoder]) {
				*rc = MOB_SNAP_FAILED;
				mob_audio_message(msg, msg_len, @"could not create the Opus encoder");
			}
		}
		if (*rc == MOB_SNAP_OK) {
			SCStreamConfiguration *c = [[[SCStreamConfiguration alloc] init] autorelease];
			c.width = 2;
			c.height = 2;
			c.minimumFrameInterval = CMTimeMake(1, 1);
			c.queueDepth = 3;
			c.showsCursor = NO;
			if (@available(macOS 13.0, *)) {
				c.capturesAudio = YES;
				c.sampleRate = MOB_RATE;
				c.channelCount = MOB_CH;
				c.excludesCurrentProcessAudio = YES;
			}
			SCContentFilter *filter = [[[SCContentFilter alloc] initWithDisplay:display excludingApplications:mob exceptingWindows:@[]] autorelease];
			a->stream = [[SCStream alloc] initWithFilter:filter configuration:c delegate:a];
			if (mob_audio_screen == nil) mob_audio_screen = [[MobAudioScreen alloc] init];
			NSError *addErr = nil;
			if (![a->stream addStreamOutput:mob_audio_screen type:SCStreamOutputTypeScreen sampleHandlerQueue:a->queue error:&addErr]) {
				*rc = mob_audio_classify(addErr, msg, msg_len);
			} else if (@available(macOS 13.0, *)) {
				if (![a->stream addStreamOutput:a type:SCStreamOutputTypeAudio sampleHandlerQueue:a->queue error:&addErr]) {
					*rc = mob_audio_classify(addErr, msg, msg_len);
				}
			}
		}
		if (*rc == MOB_SNAP_OK) {
			__block NSError *startErr = nil;
			dispatch_semaphore_t started = dispatch_semaphore_create(0);
			[a->stream startCaptureWithCompletionHandler:^(NSError *error) {
				if (error) startErr = [error retain];
				dispatch_semaphore_signal(started);
			}];
			if (dispatch_semaphore_wait(started, deadline) != 0) {
				*rc = MOB_SNAP_TIMEOUT;
				mob_audio_message(msg, msg_len, @"starting audio capture timed out");
			} else if (startErr) {
				*rc = mob_audio_classify(startErr, msg, msg_len);
			}
			dispatch_release(started);
		}
		[display release];
		[mob release];
		[err release];
		if (*rc != MOB_SNAP_OK && a != nil) {
			a->stopped = YES;
			[a release];
			a = nil;
		}
	}
	return a;
}

void mob_audio_stop(void *p) {
	MobAudio *a = p;
	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	[a->stream stopCaptureWithCompletionHandler:^(NSError *error) { dispatch_semaphore_signal(done); }];
	dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC));
	dispatch_release(done);
	dispatch_sync(a->queue, ^{ a->stopped = YES; });
	[a release];
}
