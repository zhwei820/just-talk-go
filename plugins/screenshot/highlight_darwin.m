//go:build darwin && cgo

#include "highlight_darwin.h"

#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>

static const CGFloat kCrossSize = 32.0;
static const CGFloat kCrossWidth = 2.0;
static const CGKeyCode kEscapeKey = 53; // kVK_Escape

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

static void jt_center_on_mouse(NSPanel *panel) {
	NSPoint p = [NSEvent mouseLocation];
	[panel setFrameOrigin:NSMakePoint(p.x - kCrossSize / 2.0, p.y - kCrossSize / 2.0)];
}

void jt_cursor_highlight_run(double timeout_seconds) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
		[NSApp finishLaunching];

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
		jt_center_on_mouse(panel);
		[panel orderFrontRegardless];

		NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:timeout_seconds];
		// Stop at mouse down: the screenshot is taken on mouse up, and the cross
		// must already be gone by then or it ends up in the image.
		while ([deadline timeIntervalSinceNow] > 0 &&
			([NSEvent pressedMouseButtons] & 1) == 0 &&
			!CGEventSourceKeyState(kCGEventSourceStateHIDSystemState, kEscapeKey)) {
			jt_center_on_mouse(panel);
			NSEvent *event = [NSApp nextEventMatchingMask:NSEventMaskAny
				untilDate:[NSDate dateWithTimeIntervalSinceNow:1.0 / 120.0]
				inMode:NSDefaultRunLoopMode
				dequeue:YES];
			if (event) [NSApp sendEvent:event];
		}
		[panel orderOut:nil];
		[panel close];
	}
}
