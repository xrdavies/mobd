// Screen snapshots with ScreenCaptureKit, encoded with ImageIO. Manual reference counting: the
// package's cgo flags also apply to plain C files, so ARC is not enabled.
#import <Foundation/Foundation.h>
#import <ImageIO/ImageIO.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#include <stdlib.h>
#include <string.h>
#include "snapshot_darwin.h"

// Results land in a box retained by the completion blocks, so a block that finishes after the
// caller timed out writes to live memory instead of a returned stack frame.
@interface MobSnapBox : NSObject {
@public
	SCDisplay *display;
	CGImageRef image;
	NSError *error;
}
@end

@implementation MobSnapBox
- (void)dealloc {
	[display release];
	if (image) CGImageRelease(image);
	[error release];
	[super dealloc];
}
@end

static void mob_message(char *msg, int len, NSString *s) {
	if (len > 0) snprintf(msg, len, "%s", s.UTF8String ?: "");
}

static int mob_classify(NSError *e, char *msg, int len) {
	mob_message(msg, len, e.localizedDescription);
	if (![e.domain isEqualToString:SCStreamErrorDomain]) return MOB_SNAP_FAILED;
	switch (e.code) {
	case -3801: return MOB_SNAP_DENIED;    // user declined TCC
	case -3813: case -3814: case -3815:    // no window list / display list / capture source
		return MOB_SNAP_NO_DISPLAY;
	default: return MOB_SNAP_FAILED;
	}
}

static double mob_main_scale(void) {
	CGDisplayModeRef m = CGDisplayCopyDisplayMode(CGMainDisplayID());
	if (m == NULL) return 1.0;
	size_t pw = CGDisplayModeGetPixelWidth(m), w = CGDisplayModeGetWidth(m);
	CGDisplayModeRelease(m);
	return w ? (double)pw / (double)w : 1.0;
}

static int mob_encode(CGImageRef image, int quality, void **out, long *out_len) {
	CFMutableDataRef data = CFDataCreateMutable(NULL, 0);
	CFStringRef type = quality > 0 ? CFSTR("public.jpeg") : CFSTR("public.png");
	CGImageDestinationRef dst = CGImageDestinationCreateWithData(data, type, 1, NULL);
	if (dst == NULL) {
		CFRelease(data);
		return 0;
	}
	NSDictionary *props = quality > 0 ? @{(NSString *)kCGImageDestinationLossyCompressionQuality: @(quality / 100.0)} : nil;
	CGImageDestinationAddImage(dst, image, (CFDictionaryRef)props);
	int ok = CGImageDestinationFinalize(dst);
	CFRelease(dst);
	if (ok) {
		*out_len = CFDataGetLength(data);
		*out = malloc(*out_len);
		memcpy(*out, CFDataGetBytePtr(data), *out_len);
	}
	CFRelease(data);
	return ok;
}

static int mob_snapshot14(int max_width, int quality, double timeout_seconds,
	void **out, long *out_len, int *out_width, int *out_height, char *msg, int msg_len) API_AVAILABLE(macos(14.0)) {
	int rc = MOB_SNAP_OK;
	@autoreleasepool {
		MobSnapBox *box = [[MobSnapBox alloc] init];
		dispatch_time_t deadline = dispatch_time(DISPATCH_TIME_NOW, (int64_t)(timeout_seconds * NSEC_PER_SEC));
		dispatch_semaphore_t done = dispatch_semaphore_create(0);
		[SCShareableContent getShareableContentExcludingDesktopWindows:NO onScreenWindowsOnly:YES
			completionHandler:^(SCShareableContent *content, NSError *error) {
				if (error) {
					box->error = [error retain];
				} else {
					CGDirectDisplayID main = CGMainDisplayID();
					for (SCDisplay *d in content.displays) {
						if (d.displayID == main) box->display = [d retain];
					}
				}
				dispatch_semaphore_signal(done);
			}];
		if (dispatch_semaphore_wait(done, deadline) != 0) {
			rc = MOB_SNAP_TIMEOUT;
		} else if (box->error) {
			rc = mob_classify(box->error, msg, msg_len);
		} else if (box->display == nil) {
			rc = MOB_SNAP_NO_DISPLAY;
			mob_message(msg, msg_len, @"the main display is not capturable");
		}
		if (rc == MOB_SNAP_OK) {
			double scale = mob_main_scale();
			long w = lround(box->display.width * scale), h = lround(box->display.height * scale);
			if (max_width > 0 && w > max_width) {
				h = MAX(1, h * max_width / w);
				w = max_width;
			}
			SCContentFilter *filter = [[[SCContentFilter alloc] initWithDisplay:box->display excludingWindows:@[]] autorelease];
			SCStreamConfiguration *config = [[[SCStreamConfiguration alloc] init] autorelease];
			config.width = w;
			config.height = h;
			config.showsCursor = NO; // like screencapture -x
			dispatch_semaphore_t shot = dispatch_semaphore_create(0);
			[SCScreenshotManager captureImageWithFilter:filter configuration:config
				completionHandler:^(CGImageRef image, NSError *error) {
					if (error) box->error = [error retain];
					else if (image) box->image = CGImageRetain(image);
					dispatch_semaphore_signal(shot);
				}];
			if (dispatch_semaphore_wait(shot, deadline) != 0) {
				rc = MOB_SNAP_TIMEOUT;
			} else if (box->error) {
				rc = mob_classify(box->error, msg, msg_len);
			} else if (box->image == NULL || !mob_encode(box->image, quality, out, out_len)) {
				rc = MOB_SNAP_FAILED;
				mob_message(msg, msg_len, @"capture produced no image");
			} else {
				*out_width = (int)CGImageGetWidth(box->image);
				*out_height = (int)CGImageGetHeight(box->image);
			}
			dispatch_release(shot);
		}
		dispatch_release(done);
		[box release];
	}
	if (rc == MOB_SNAP_TIMEOUT) mob_message(msg, msg_len, @"screen capture timed out");
	return rc;
}

int mob_snapshot(int max_width, int quality, double timeout_seconds,
	void **out, long *out_len, int *out_width, int *out_height, char *msg, int msg_len) {
	if (@available(macOS 14.0, *)) {
		return mob_snapshot14(max_width, quality, timeout_seconds, out, out_len, out_width, out_height, msg, msg_len);
	}
	return MOB_SNAP_UNAVAILABLE;
}

void mob_display_state(int *locked, int *asleep) {
	*locked = 0;
	CFDictionaryRef session = CGSessionCopyCurrentDictionary();
	if (session != NULL) {
		CFBooleanRef v = CFDictionaryGetValue(session, CFSTR("CGSSessionScreenIsLocked"));
		*locked = v != NULL && CFBooleanGetValue(v);
		CFRelease(session);
	}
	*asleep = CGDisplayIsAsleep(CGMainDisplayID()) != 0;
}
