//go:build darwin && cgo

#include "highlight_darwin.h"
#include "annotate_darwin.h"

#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#include <math.h>
#include <unistd.h>

static const CGFloat kCrossSize = 32.0;
static const CGFloat kCrossWidth = 2.0;
static const CGKeyCode kEscapeKey = 53; // kVK_Escape
static const NSTimeInterval kFullScreenImageTimeout = 2.0;

@interface JTCursorCrossView : NSView
@end

@implementation JTCursorCrossView
- (BOOL)isOpaque { return NO; }
- (void)drawRect:(NSRect)dirtyRect {
	(void)dirtyRect;
	NSRect b = [self bounds];
	CGFloat cx = NSMidX(b), cy = NSMidY(b);
	NSBezierPath *path = [NSBezierPath bezierPath];
	[path moveToPoint:NSMakePoint(NSMinX(b) + 1.0, cy)];
	[path lineToPoint:NSMakePoint(NSMaxX(b) - 1.0, cy)];
	[path moveToPoint:NSMakePoint(cx, NSMinY(b) + 1.0)];
	[path lineToPoint:NSMakePoint(cx, NSMaxY(b) - 1.0)];
	// White halo first so the red cross stays visible on red backgrounds.
	[path setLineWidth:kCrossWidth + 2.0];
	[[NSColor colorWithCalibratedWhite:1.0 alpha:0.9] setStroke];
	[path stroke];
	[path setLineWidth:kCrossWidth];
	[[NSColor colorWithCalibratedRed:1.0 green:0.15 blue:0.2 alpha:1.0] setStroke];
	[path stroke];
}
@end

int jt_post_screenshot_shortcut(int key) {
	const CGEventFlags flags = kCGEventFlagMaskControl | kCGEventFlagMaskShift | kCGEventFlagMaskCommand;
	CGEventRef down = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)key, true);
	CGEventRef up = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)key, false);
	if (!down || !up) {
		if (down) CFRelease(down);
		if (up) CFRelease(up);
		return -1;
	}
	CGEventSetFlags(down, flags);
	CGEventSetFlags(up, flags);
	CGEventPost(kCGHIDEventTap, down);
	CGEventPost(kCGHIDEventTap, up);
	CFRelease(down);
	CFRelease(up);
	return 0;
}

static NSArray *jt_pasteboard_snapshot(NSPasteboard *pb) {
	NSMutableArray *snap = [NSMutableArray array];
	for (NSPasteboardItem *item in [pb pasteboardItems]) {
		NSMutableDictionary *types = [NSMutableDictionary dictionary];
		for (NSString *type in [item types]) {
			NSData *data = [item dataForType:type];
			if (data) types[type] = data;
		}
		[snap addObject:types];
	}
	return snap;
}

static void jt_pasteboard_restore(NSPasteboard *pb, NSArray *snap) {
	[pb clearContents];
	NSMutableArray *items = [NSMutableArray arrayWithCapacity:[snap count]];
	for (NSDictionary *types in snap) {
		NSPasteboardItem *item = [[[NSPasteboardItem alloc] init] autorelease];
		for (NSString *type in types) [item setData:types[type] forType:type];
		[items addObject:item];
	}
	if ([items count] > 0) [pb writeObjects:items];
}

// ⌃⇧⌘3 copies the main display to the pasteboard through the system
// screenshot UI, so no Screen Recording permission is needed.
static NSImage *jt_copy_full_screen(NSPasteboard *pb) {
	NSInteger before = [pb changeCount];
	if (jt_post_screenshot_shortcut(JT_SCREENSHOT_KEY_FULL) != 0) return nil;
	NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:kFullScreenImageTimeout];
	while ([deadline timeIntervalSinceNow] > 0) {
		if ([pb changeCount] != before) {
			NSImage *image = [[[NSImage alloc] initWithPasteboard:pb] autorelease];
			if (image) return image;
		}
		usleep(10 * 1000);
	}
	return nil;
}

static NSScreen *jt_screen_for_image(NSImage *image) {
	NSImageRep *rep = [[image representations] firstObject];
	if (!rep) return nil;
	for (NSScreen *screen in [NSScreen screens]) {
		NSSize pts = [screen frame].size;
		CGFloat scale = [screen backingScaleFactor];
		if ((NSInteger)llround(pts.width * scale) == [rep pixelsWide] &&
			(NSInteger)llround(pts.height * scale) == [rep pixelsHigh]) {
			return screen;
		}
	}
	return nil;
}

// The frozen image is a normal window, so the region the user selects is cut
// from it rather than from the live screen underneath.
static NSWindow *jt_show_frozen_screen(NSImage *image, NSScreen *screen) {
	NSRect frame = [screen frame];
	[image setSize:frame.size];
	NSWindow *window = [[NSWindow alloc] initWithContentRect:frame
		styleMask:NSWindowStyleMaskBorderless
		backing:NSBackingStoreBuffered
		defer:NO];
	[window setFrame:frame display:NO];
	[window setOpaque:YES];
	[window setHasShadow:NO];
	[window setIgnoresMouseEvents:YES];
	// Above the menu bar and Dock, below the system screenshot UI.
	[window setLevel:NSScreenSaverWindowLevel];
	[window setCollectionBehavior:NSWindowCollectionBehaviorCanJoinAllSpaces |
		NSWindowCollectionBehaviorFullScreenAuxiliary |
		NSWindowCollectionBehaviorStationary];
	NSImageView *view = [[[NSImageView alloc] initWithFrame:NSMakeRect(0, 0, frame.size.width, frame.size.height)] autorelease];
	[view setImageScaling:NSImageScaleAxesIndependently];
	[view setImage:image];
	[window setContentView:view];
	[window orderFrontRegardless];
	[window display];
	return window;
}

static NSPanel *jt_make_cross_panel(void) {
	NSPanel *panel = [[NSPanel alloc] initWithContentRect:NSMakeRect(0, 0, kCrossSize, kCrossSize)
		styleMask:NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel
		backing:NSBackingStoreBuffered
		defer:NO];
	[panel setOpaque:NO];
	[panel setBackgroundColor:[NSColor clearColor]];
	[panel setHasShadow:NO];
	[panel setIgnoresMouseEvents:YES];
	// The system screenshot UI sits above normal floating windows.
	[panel setLevel:CGShieldingWindowLevel() + 1];
	[panel setSharingType:NSWindowSharingNone];
	[panel setCollectionBehavior:NSWindowCollectionBehaviorCanJoinAllSpaces |
		NSWindowCollectionBehaviorFullScreenAuxiliary |
		NSWindowCollectionBehaviorStationary];
	[panel setContentView:[[[JTCursorCrossView alloc] initWithFrame:NSMakeRect(0, 0, kCrossSize, kCrossSize)] autorelease]];
	return panel;
}

static void jt_center_on_mouse(NSPanel *panel) {
	NSPoint p = [NSEvent mouseLocation];
	[panel setFrameOrigin:NSMakePoint(p.x - kCrossSize / 2.0, p.y - kCrossSize / 2.0)];
}

static void jt_pump_events(NSTimeInterval seconds) {
	NSEvent *event = [NSApp nextEventMatchingMask:NSEventMaskAny
		untilDate:[NSDate dateWithTimeIntervalSinceNow:seconds]
		inMode:NSDefaultRunLoopMode
		dequeue:YES];
	if (event) [NSApp sendEvent:event];
}

int jt_screenshot_ui_run(double timeout_seconds) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
		[NSApp finishLaunching];

		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		NSArray *original = jt_pasteboard_snapshot(pb);
		NSInteger originalCount = [pb changeCount];

		// Without a frozen image the selection falls back to the live screen.
		NSWindow *frozen = nil;
		NSImage *image = jt_copy_full_screen(pb);
		NSScreen *screen = image ? jt_screen_for_image(image) : nil;
		if (screen) {
			frozen = jt_show_frozen_screen(image, screen);
			jt_pump_events(0.05);
		}

		NSPanel *cross = jt_make_cross_panel();
		jt_center_on_mouse(cross);
		[cross orderFrontRegardless];

		NSInteger selectionBase = [pb changeCount];
		// Filled in while the user drags the selection, read again after it finishes.
		NSPoint dragStart = NSZeroPoint, dragEnd = NSZeroPoint;
		int result = JT_SCREENSHOT_UI_CANCELLED;
		if (jt_post_screenshot_shortcut(JT_SCREENSHOT_KEY_REGION) != 0) {
			result = JT_SCREENSHOT_UI_FAILED;
		} else {
			NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:timeout_seconds];
			BOOL crossVisible = YES;
			BOOL mouseDown = NO;
			while ([deadline timeIntervalSinceNow] > 0 &&
				!CGEventSourceKeyState(kCGEventSourceStateHIDSystemState, kEscapeKey)) {
				// The system UI writes the selected region to the pasteboard,
				// which is the only observable sign that the capture finished.
				// It clears the pasteboard first and writes the image a moment
				// later, so a changed count alone may still mean an empty board.
				if ([pb changeCount] != selectionBase && [NSImage canInitWithPasteboard:pb]) {
					result = JT_SCREENSHOT_UI_DONE;
					break;
				}
				if (crossVisible) {
					// Hide at mouse down: the screenshot is taken on mouse up,
					// and the cross must already be gone by then or it ends up
					// in the image.
					if (([NSEvent pressedMouseButtons] & 1) != 0) {
						// Mouse down is the one corner the system keeps, even when a
						// modifier constrains the drag, so the region can be derived
						// from it later without asking the system for the rectangle.
						dragStart = [NSEvent mouseLocation];
						dragEnd = dragStart;
						mouseDown = YES;
						[cross orderOut:nil];
						crossVisible = NO;
					} else {
						jt_center_on_mouse(cross);
					}
				} else if (mouseDown && ([NSEvent pressedMouseButtons] & 1) != 0) {
					// Keep updating until the button is released: the last position
					// tells us which way the drag went, i.e. which corner was the start.
					dragEnd = [NSEvent mouseLocation];
				}
				jt_pump_events(1.0 / 120.0);
			}
		}

		[cross orderOut:nil];
		[cross close];
		if (result == JT_SCREENSHOT_UI_DONE) {
			// The frozen window stays up while annotating: the annotation window is
			// placed back on the region, so the frozen screen has to stay behind it.
			// It also starts swallowing mouse events here, otherwise a click outside
			// the region would reach the application behind us.
			if (frozen) [frozen setIgnoresMouseEvents:NO];
			jt_annotate_context_t context;
			context.image = image;
			context.screen = screen;
			context.drag_start = dragStart;
			context.drag_end = dragEnd;
			if (jt_annotate_capture(pb, context) == JT_ANNOTATE_CANCELLED) {
				result = JT_SCREENSHOT_UI_CANCELLED;
			}
		}
		if (frozen) {
			[frozen orderOut:nil];
			[frozen close];
		}
		if (result != JT_SCREENSHOT_UI_DONE && [pb changeCount] != originalCount) {
			jt_pasteboard_restore(pb, original);
		}
		return result;
	}
}
