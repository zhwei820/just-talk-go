//go:build darwin && cgo

#include "annotate_darwin.h"
#include "locate_darwin.h"

#import <AppKit/AppKit.h>
#include <math.h>

// 线宽口径是「5 个图像像素」而不是屏幕点：2x 图和 1x 图上量出来都是 5px。
static const CGFloat kStrokePixelWidth = 5.0;
static const CGFloat kHintBarHeight = 28.0;
static const CGFloat kHintPadding = 10.0;
static const CGFloat kHintGap = 8.0;
static const CGFloat kScreenMargin = 40.0;
// 事件循环的唤醒间隔（只在窗口还没拿到 key 时做事）与补激活的时长上限。
static const NSTimeInterval kEventSliceSeconds = 0.1;
static const NSTimeInterval kFocusRetrySeconds = 1.0;
// 拖拽小于这个尺寸（选区局部点）当成手抖的点击，不记一个退化成点的框。
static const CGFloat kMinRectPoints = 2.0;
// 判「这是双击而非画框」允许的抖动余量，比上面宽：双击时手难免动几个点。
static const CGFloat kClickSlopPoints = 8.0;

static const CGKeyCode kKeyCodeReturn = 36;      // kVK_Return
static const CGKeyCode kKeyCodeKeypadEnter = 76; // kVK_ANSI_KeypadEnter
static const CGKeyCode kKeyCodeEscape = 53;      // kVK_Escape
static const CGKeyCode kKeyCodeBackspace = 51;   // kVK_Delete

static NSString *const kHintText = @"拖拽画框 · ⌘Z 撤销 · Enter 复制 · Esc 取消";

// 用 sRGB 显式声明纯红：calibrated RGB 会被色彩管理换算（画进 sRGB / P3 位图时分量不再等于 1,0,0），
// sRGB 红则不管目标图是什么空间，视觉上都是「正红」。
static NSColor *jt_stroke_color(void) {
	return [NSColor colorWithSRGBRed:1.0 green:0.0 blue:0.0 alpha:1.0];
}

static NSColor *jt_hint_bar_color(void) {
	return [NSColor colorWithCalibratedWhite:0.12 alpha:1.0];
}

static NSDictionary *jt_hint_attributes(void) {
	return [NSDictionary dictionaryWithObjectsAndKeys:
		[NSFont systemFontOfSize:12.0], NSFontAttributeName,
		[NSColor colorWithCalibratedWhite:0.82 alpha:1.0], NSForegroundColorAttributeName,
		nil];
}

// 描边只有这一处实现：屏幕上看到的预览和最后烧进图里的框走同一个函数、同一套线宽换算，
// 所以「拖出来的样子」就是「复制出去的样子」。rect 与 line_width 都按调用方的坐标系给。
static void jt_stroke_rect(NSRect rect, CGFloat line_width) {
	NSBezierPath *path = [NSBezierPath bezierPathWithRect:rect];
	[path setLineWidth:line_width];
	[path stroke];
}

NSImage *jt_composite_rects(NSImage *image, NSArray *rects, CGFloat image_scale) {
	if (image == nil || image_scale <= 0.0) return nil;
	CGImageRef source = jt_image_cgimage(image);
	if (source == NULL) return nil;
	size_t pixel_width = CGImageGetWidth(source);
	size_t pixel_height = CGImageGetHeight(source);
	if (pixel_width == 0 || pixel_height == 0) return nil;

	// 位图沿用源图的色彩空间：换成设备 RGB 会把 P3 截图里的饱和色压掉。
	CGColorSpaceRef space = CGImageGetColorSpace(source);
	CGContextRef bitmap = space == NULL ? NULL
		: CGBitmapContextCreate(NULL, pixel_width, pixel_height, 8, 0, space, (CGBitmapInfo)kCGImageAlphaPremultipliedFirst);
	if (bitmap == NULL) {
		CGColorSpaceRef fallback = CGColorSpaceCreateDeviceRGB();
		bitmap = CGBitmapContextCreate(NULL, pixel_width, pixel_height, 8, 0, fallback, (CGBitmapInfo)kCGImageAlphaPremultipliedFirst);
		CGColorSpaceRelease(fallback);
	}
	if (bitmap == NULL) return nil;

	// 源图按原始像素 1:1 画入，不走缩放。
	CGContextDrawImage(bitmap, CGRectMake(0, 0, (CGFloat)pixel_width, (CGFloat)pixel_height), source);
	// 之后的用户坐标是图像点，线宽除以缩放比才落回 5 个真实像素。
	CGContextScaleCTM(bitmap, image_scale, image_scale);

	NSGraphicsContext *ctx = [NSGraphicsContext graphicsContextWithCGContext:bitmap flipped:NO];
	[NSGraphicsContext saveGraphicsState];
	[NSGraphicsContext setCurrentContext:ctx];
	[jt_stroke_color() setStroke];
	for (NSValue *value in rects) {
		jt_stroke_rect([value rectValue], kStrokePixelWidth / image_scale);
	}
	[NSGraphicsContext restoreGraphicsState];

	CGImageRef composed = CGBitmapContextCreateImage(bitmap);
	CGContextRelease(bitmap);
	if (composed == NULL) return nil;

	NSSize point_size = NSMakeSize(pixel_width / image_scale, pixel_height / image_scale);
	NSBitmapImageRep *rep = [[[NSBitmapImageRep alloc] initWithCGImage:composed] autorelease];
	CGImageRelease(composed);
	// size 用点表示，导出的 PNG 才会带上和源图一样的 dpi，粘到别处尺寸不变。
	[rep setSize:point_size];
	NSImage *out = [[[NSImage alloc] initWithSize:point_size] autorelease];
	[out addRepresentation:rep];
	return out;
}

typedef struct {
	NSRect frame;          // 窗口 / 视图尺寸
	NSRect image_frame;    // 图像在视图里的位置
	NSRect hint_rect;      // 提示条位置（视图坐标）；空矩形 = 提示条画在窗口外，窗口里不画
	NSSize image_points;   // 图像自身的点尺寸
	CGFloat display_scale; // 视图点 / 图像点
} jt_annotate_layout_t;

// 原地模式：窗口就是选区那一块、图 1:1 铺满，提示条由冻结层上的蒙版画。
static jt_annotate_layout_t jt_annotate_layout_in_place(NSSize image_points) {
	jt_annotate_layout_t layout;
	layout.display_scale = 1.0;
	layout.image_points = image_points;
	layout.frame = NSMakeRect(0, 0, image_points.width, image_points.height);
	layout.image_frame = layout.frame;
	layout.hint_rect = NSZeroRect;
	return layout;
}

// 兜底模式：定位不到选区时用的居中浮窗。图按自身点尺寸显示，比屏幕可见区还大时等比缩小；
// 窄图放不下提示条文字时把窗口加宽，图在窗口里居中。
static jt_annotate_layout_t jt_annotate_layout_floating(NSSize image_points, NSScreen *screen) {
	jt_annotate_layout_t layout;
	NSRect visible = [screen visibleFrame];
	CGFloat hint_width = ceil([kHintText sizeWithAttributes:jt_hint_attributes()].width) + 2.0 * kHintPadding;
	CGFloat max_width = fmax(NSWidth(visible) - 2.0 * kScreenMargin, hint_width);
	CGFloat max_height = NSHeight(visible) - 2.0 * kScreenMargin - kHintBarHeight;
	CGFloat fit = fmin(1.0, fmin(max_width / image_points.width, max_height / image_points.height));

	NSSize shown = NSMakeSize(image_points.width * fit, image_points.height * fit);
	NSSize content = NSMakeSize(fmax(shown.width, hint_width), shown.height + kHintBarHeight);
	layout.display_scale = fit;
	layout.image_points = image_points;
	layout.frame = NSMakeRect(NSMidX(visible) - content.width / 2.0,
		NSMidY(visible) - content.height / 2.0,
		content.width, content.height);
	layout.image_frame = NSMakeRect((content.width - shown.width) / 2.0, kHintBarHeight, shown.width, shown.height);
	layout.hint_rect = NSMakeRect(0, 0, content.width, kHintBarHeight);
	return layout;
}

static NSScreen *jt_screen_containing_point(NSPoint point) {
	for (NSScreen *screen in [NSScreen screens]) {
		if (NSPointInRect(point, [screen frame])) return screen;
	}
	return nil;
}

// 窗口边界落在半像素上时，AppKit 会在它成为 key window 时自己对齐一次，
// 用户看到的就是「点一下、窗口飘一下」。这里先对齐到整像素，让它没什么可对齐的。
static NSRect jt_pixel_aligned_rect(NSRect rect, CGFloat scale) {
	return NSMakeRect(round(NSMinX(rect) * scale) / scale, round(NSMinY(rect) * scale) / scale,
		round(NSWidth(rect) * scale) / scale, round(NSHeight(rect) * scale) / scale);
}

typedef struct {
	jt_annotate_layout_t layout;
	NSPoint window_origin; // 窗口左下角（屏幕坐标）
	NSRect region_screen;  // 选区在屏幕上的位置
	BOOL in_place;
} jt_annotate_placement_t;

// 能定位就把标注窗摆回选区原地，否则退回居中浮窗。
static jt_annotate_placement_t jt_plan_annotation(jt_annotate_context_t context, NSImage *region_image, CGFloat scale) {
	jt_annotate_placement_t placement;
	placement.in_place = NO;
	placement.window_origin = NSZeroPoint;
	placement.region_screen = NSZeroRect;

	CGImageRef region_cg = jt_image_cgimage(region_image);
	NSSize region_px = NSMakeSize(CGImageGetWidth(region_cg), CGImageGetHeight(region_cg));
	NSSize region_points = NSMakeSize(region_px.width / scale, region_px.height / scale);
	NSPoint origin_top_px = NSZeroPoint;

	// 冻结屏信息缺一就没法定位（比如系统那边冻结失败），直接走兜底
	if (context.image != nil && context.screen != nil) {
		NSRect screen_frame = [context.screen frame];
		NSPoint hint_top_px = jt_hint_origin_top_px(context.drag_start, context.drag_end, screen_frame, region_px, scale);
		if (jt_locate_region(context.image, region_image, hint_top_px, &origin_top_px)) {
			placement.region_screen = jt_pixel_aligned_rect(NSMakeRect(NSMinX(screen_frame) + origin_top_px.x / scale,
				NSMaxY(screen_frame) - (origin_top_px.y + region_px.height) / scale,
				region_points.width, region_points.height), scale);
			placement.layout = jt_annotate_layout_in_place(region_points);
			placement.window_origin = placement.region_screen.origin;
			placement.in_place = YES;
			return placement;
		}
	}

	NSScreen *screen = context.screen;
	if (screen == nil) screen = jt_screen_containing_point([NSEvent mouseLocation]);
	if (screen == nil) screen = [NSScreen mainScreen];

	placement.layout = jt_annotate_layout_floating(region_points, screen);
	placement.window_origin = placement.layout.frame.origin;
	return placement;
}

@interface JTAnnotateView : NSView {
	NSImage *_image;           // 框选出来的区域图
	NSMutableArray *_rects;    // 已确认的框，图像点坐标
	NSRect _imageFrame;        // 图像在视图里的位置（视图坐标）
	NSRect _hintRect;          // 提示条位置；空矩形表示提示条画在窗口外
	NSRect _dragRect;          // 正在拖的框，选区局部点坐标
	NSPoint _dragOrigin;       // 拖拽起点，图像点坐标
	CGFloat _displayScale;     // 视图点 / 图像点
	CGFloat _strokeWidth;      // 预览线宽，视图点
	BOOL _isDragging;
	BOOL _isFinished;
	BOOL _isCommitted;
}
- (id)initWithImage:(NSImage *)image layout:(jt_annotate_layout_t)layout strokeWidth:(CGFloat)stroke_width;
- (NSArray *)rects;
- (BOOL)isFinished;
- (BOOL)isCommitted;
@end

@implementation JTAnnotateView

- (id)initWithImage:(NSImage *)image layout:(jt_annotate_layout_t)layout strokeWidth:(CGFloat)stroke_width {
	self = [super initWithFrame:layout.frame];
	if (self == nil) return nil;
	_image = [image retain];
	_rects = [[NSMutableArray alloc] init];
	_imageFrame = layout.image_frame;
	_hintRect = layout.hint_rect;
	_displayScale = layout.display_scale;
	_strokeWidth = stroke_width;
	return self;
}

- (void)dealloc {
	[_image release];
	[_rects release];
	[super dealloc];
}

- (NSArray *)rects { return _rects; }
- (BOOL)isFinished { return _isFinished; }
- (BOOL)isCommitted { return _isCommitted; }
- (BOOL)acceptsFirstResponder { return YES; }

// 窗口刚出现时应用可能还没激活（系统截图 UI 退场时会把焦点还给原来的 app），
// 而默认行为会把「激活应用」的那一次点击吃掉——第一笔就画不出来。这里让它直接生效。
- (BOOL)acceptsFirstMouse:(NSEvent *)event {
	(void)event;
	return YES;
}

- (void)resetCursorRects {
	// 鼠标进到这块区域就换成小十字：这里是可以画的
	[self addCursorRect:[self bounds] cursor:[NSCursor crosshairCursor]];
}

- (void)drawRect:(NSRect)dirtyRect {
	(void)dirtyRect;
	[jt_hint_bar_color() setFill];
	NSRectFill([self bounds]);

	NSDictionary *hints = [NSDictionary dictionaryWithObject:[NSNumber numberWithInt:NSImageInterpolationHigh]
		forKey:NSImageHintInterpolation];
	[_image drawInRect:_imageFrame fromRect:NSZeroRect operation:NSCompositingOperationCopy fraction:1.0
		respectFlipped:YES hints:hints];

	[jt_stroke_color() setStroke];
	// 框存的是「图内坐标」，画的时候要加回图在窗里的位置——原地模式是 (0,0)，
	// 兜底模式才在提示条上方。写死偏移会让预览框和鼠标错位。
	for (NSValue *value in _rects) {
		jt_stroke_rect(NSOffsetRect([value rectValue], NSMinX(_imageFrame), NSMinY(_imageFrame)), _strokeWidth);
	}
	if (_isDragging) {
		// 预览和最终结果用同样的描边，所见即所得
		jt_stroke_rect(NSOffsetRect(_dragRect, NSMinX(_imageFrame), NSMinY(_imageFrame)), _strokeWidth);
	}
	if (!NSIsEmptyRect(_hintRect)) {
		[kHintText drawAtPoint:NSMakePoint(NSMinX(_hintRect) + kHintPadding,
			NSMinY(_hintRect) + (kHintBarHeight - 16.0) / 2.0) withAttributes:jt_hint_attributes()];
	}
}

// 视图坐标 → 图像点坐标，并夹在图像范围内（拖出图外不会画出越界的框）。
- (NSPoint)imagePointFromEvent:(NSEvent *)event {
	NSPoint view_point = [self convertPoint:[event locationInWindow] fromView:nil];
	NSSize image_points = NSMakeSize(NSWidth(_imageFrame) / _displayScale, NSHeight(_imageFrame) / _displayScale);
	CGFloat x = fmin(fmax((view_point.x - NSMinX(_imageFrame)) / _displayScale, 0.0), image_points.width);
	CGFloat y = fmin(fmax((view_point.y - NSMinY(_imageFrame)) / _displayScale, 0.0), image_points.height);
	return NSMakePoint(x, y);
}

- (void)mouseDown:(NSEvent *)event {
	NSPoint view_point = [self convertPoint:[event locationInWindow] fromView:nil];
	if (!NSPointInRect(view_point, _imageFrame)) return; // 点在提示条上：不画
	_dragOrigin = [self imagePointFromEvent:event];
	_dragRect = NSMakeRect(_dragOrigin.x, _dragOrigin.y, 0, 0);
	_isDragging = YES;
	[self setNeedsDisplay:YES];
}

- (void)mouseDragged:(NSEvent *)event {
	if (!_isDragging) return;
	[self updateDragToPoint:[self imagePointFromEvent:event]];
}

- (void)mouseUp:(NSEvent *)event {
	if (!_isDragging) return;
	[self updateDragToPoint:[self imagePointFromEvent:event]];
	_isDragging = NO;
	// 双击 = 复制，但必须先确认「没真的拖动」：连画两个框时，第二次按下离上一次又近又急，
	// 系统会把它记成 clickCount=2——只看 clickCount 就会把第二个框当成提交，标注窗直接退出。
	if ([event clickCount] >= 2 && _dragRect.size.width < kClickSlopPoints && _dragRect.size.height < kClickSlopPoints) {
		_dragRect = NSZeroRect;
		_isCommitted = YES;
		_isFinished = YES;
		return;
	}
	if (_dragRect.size.width >= kMinRectPoints && _dragRect.size.height >= kMinRectPoints) {
		[_rects addObject:[NSValue valueWithRect:_dragRect]];
	}
	_dragRect = NSZeroRect;
	[self setNeedsDisplay:YES];
}

- (void)rightMouseDown:(NSEvent *)event {
	(void)event;
	_isFinished = YES;
}

- (void)updateDragToPoint:(NSPoint)point {
	_dragRect = NSMakeRect(fmin(point.x, _dragOrigin.x), fmin(point.y, _dragOrigin.y),
		fabs(point.x - _dragOrigin.x), fabs(point.y - _dragOrigin.y));
	[self setNeedsDisplay:YES];
}

- (void)keyDown:(NSEvent *)event {
	switch ([event keyCode]) {
	case kKeyCodeReturn:
	case kKeyCodeKeypadEnter:
		_isCommitted = YES;
		_isFinished = YES;
		break;
	case kKeyCodeEscape:
		_isFinished = YES;
		break;
	case kKeyCodeBackspace:
		[self removeLastRect];
		break;
	default:
		[super keyDown:event];
		break;
	}
}

// 带 command 的按键进不了 keyDown，会先走这里。
- (BOOL)performKeyEquivalent:(NSEvent *)event {
	if (([event modifierFlags] & NSEventModifierFlagCommand) != 0 &&
		[[[event charactersIgnoringModifiers] lowercaseString] isEqualToString:@"z"]) {
		[self removeLastRect];
		return YES;
	}
	return NO;
}

- (void)removeLastRect {
	if ([_rects count] == 0) return;
	[_rects removeLastObject];
	[self setNeedsDisplay:YES];
}

@end

@interface JTAnnotateWindow : NSWindow
@end

@implementation JTAnnotateWindow

// 无边框窗默认不接键盘，不覆盖这个的话 Enter / Esc / ⌘Z 全都收不到。
- (BOOL)canBecomeKeyWindow { return YES; }

@end

// 压在冻结屏上的蒙版：选区外压暗、给选区描边、把提示条贴在选区旁边。不接鼠标。
@interface JTVeilView : NSView {
	NSRect _region; // 选区（视图坐标）
}
- (id)initWithFrame:(NSRect)frame region:(NSRect)region;
@end

@implementation JTVeilView

- (id)initWithFrame:(NSRect)frame region:(NSRect)region {
	self = [super initWithFrame:frame];
	if (self != nil) _region = region;
	return self;
}

- (BOOL)isOpaque { return NO; }

- (void)drawRect:(NSRect)dirtyRect {
	(void)dirtyRect;
	NSRect bounds = [self bounds];

	[[NSColor colorWithCalibratedWhite:0.0 alpha:0.35] setFill];
	CGFloat region_height = NSHeight(_region);
	NSRectFill(NSMakeRect(NSMinX(bounds), NSMaxY(_region), NSWidth(bounds), NSMaxY(bounds) - NSMaxY(_region)));
	NSRectFill(NSMakeRect(NSMinX(bounds), NSMinY(bounds), NSWidth(bounds), NSMinY(_region) - NSMinY(bounds)));
	NSRectFill(NSMakeRect(NSMinX(bounds), NSMinY(_region), NSMinX(_region) - NSMinX(bounds), region_height));
	NSRectFill(NSMakeRect(NSMaxX(_region), NSMinY(_region), NSMaxX(bounds) - NSMaxX(_region), region_height));

	[[NSColor colorWithCalibratedWhite:1.0 alpha:0.85] setStroke];
	NSBezierPath *border = [NSBezierPath bezierPathWithRect:NSInsetRect(_region, -0.5, -0.5)];
	[border setLineWidth:1.0];
	[border stroke];

	// 提示条优先贴选区下方，下方放不下就上方，都放不下就压进选区内部底边
	NSDictionary *attributes = jt_hint_attributes();
	NSSize text = [kHintText sizeWithAttributes:attributes];
	NSSize pill = NSMakeSize(text.width + 2.0 * kHintPadding, text.height + 8.0);
	CGFloat x = fmin(fmax(NSMinX(_region), NSMinX(bounds)), NSMaxX(bounds) - pill.width);
	CGFloat y = NSMinY(_region) + kHintGap;
	if (NSMinY(_region) - kHintGap - pill.height >= NSMinY(bounds)) {
		y = NSMinY(_region) - kHintGap - pill.height;
	} else if (NSMaxY(_region) + kHintGap + pill.height <= NSMaxY(bounds)) {
		y = NSMaxY(_region) + kHintGap;
	}
	NSBezierPath *background = [NSBezierPath bezierPathWithRoundedRect:NSMakeRect(x, y, pill.width, pill.height)
		xRadius:6.0 yRadius:6.0];
	[[NSColor colorWithCalibratedWhite:0.1 alpha:0.8] setFill];
	[background fill];
	[kHintText drawAtPoint:NSMakePoint(x + kHintPadding, y + 4.0) withAttributes:attributes];
}

@end

// 无边框、压在菜单栏与 Dock 之上、跟着用户跨 Space 显示。
static JTAnnotateWindow *jt_make_overlay_window(NSRect frame) {
	JTAnnotateWindow *window = [[JTAnnotateWindow alloc] initWithContentRect:frame
		styleMask:NSWindowStyleMaskBorderless
		backing:NSBackingStoreBuffered
		defer:NO];
	[window setReleasedWhenClosed:NO];
	[window setLevel:NSScreenSaverWindowLevel];
	[window setCollectionBehavior:NSWindowCollectionBehaviorCanJoinAllSpaces |
		NSWindowCollectionBehaviorFullScreenAuxiliary |
		NSWindowCollectionBehaviorStationary];
	return window;
}

static JTAnnotateWindow *jt_make_veil_window(NSScreen *screen, NSRect region_screen) {
	NSRect frame = [screen frame];
	JTAnnotateWindow *window = jt_make_overlay_window(frame);
	[window setOpaque:NO];
	[window setBackgroundColor:[NSColor clearColor]];
	[window setHasShadow:NO];
	[window setIgnoresMouseEvents:YES];
	[window setContentView:[[[JTVeilView alloc] initWithFrame:NSMakeRect(0, 0, NSWidth(frame), NSHeight(frame))
		region:NSOffsetRect(region_screen, -NSMinX(frame), -NSMinY(frame))] autorelease]];
	[window orderFrontRegardless];
	return window;
}

static JTAnnotateWindow *jt_make_annotate_window(jt_annotate_layout_t layout, NSPoint origin) {
	JTAnnotateWindow *window = jt_make_overlay_window(NSOffsetRect(layout.frame, origin.x, origin.y));
	[window setOpaque:YES];
	[window setBackgroundColor:jt_hint_bar_color()];
	// 原地模式下不要阴影：它得看上去就是屏幕上那一块内容，而不是飘在空中的面板
	[window setHasShadow:!NSIsEmptyRect(layout.hint_rect)];
	return window;
}

// 提交：把框烧进图里写回剪贴板。没画框就一个字节都不动——剪贴板里本来就是系统写进去的原图。
static void jt_write_annotated_image(NSPasteboard *pasteboard, NSImage *image, NSArray *rects, CGFloat scale) {
	if ([rects count] == 0) return;
	NSImage *annotated = jt_composite_rects(image, rects, scale);
	if (annotated == nil) return;
	[pasteboard clearContents];
	if (![pasteboard writeObjects:[NSArray arrayWithObject:annotated]]) {
		// 写失败时剪贴板已经被清空，退回未标注的图——总比让用户丢掉这张截图好。
		[pasteboard clearContents];
		[pasteboard writeObjects:[NSArray arrayWithObject:image]];
	}
}

int jt_annotate_capture(NSPasteboard *pasteboard, jt_annotate_context_t context) {
	@autoreleasepool {
		NSImage *region_image = [[[NSImage alloc] initWithPasteboard:pasteboard] autorelease];
		CGFloat scale = region_image == nil ? 0.0 : jt_image_pixels_per_point(region_image);
		if (scale <= 0.0) return JT_ANNOTATE_SKIPPED;

		jt_annotate_placement_t placement = jt_plan_annotation(context, region_image, scale);
		CGFloat stroke_width = kStrokePixelWidth / scale * placement.layout.display_scale;

		JTAnnotateWindow *veil = placement.in_place
			? jt_make_veil_window(context.screen, placement.region_screen) : nil;
		JTAnnotateWindow *window = jt_make_annotate_window(placement.layout, placement.window_origin);
		JTAnnotateView *view = [[[JTAnnotateView alloc] initWithImage:region_image layout:placement.layout
			strokeWidth:stroke_width] autorelease];
		[window setContentView:view];
		[window makeFirstResponder:view];
		// 助手进程平时不抢焦点，这里必须抢一次：否则键盘事件进不到标注窗，Enter / Esc 全失灵。
		[NSApp activateIgnoringOtherApps:YES];
		[window makeKeyAndOrderFront:nil];

		// 系统截图 UI 退场时会把焦点还给原来的 app，可能正好盖掉我们刚抢到的激活；
		// 窗口出现后的一小段时间里补几次，免得窗口看得见却收不到键盘（Enter / Esc / ⌘Z）。
		NSDate *focus_deadline = [NSDate dateWithTimeIntervalSinceNow:kFocusRetrySeconds];
		while (![view isFinished]) {
			@autoreleasepool {
				NSEvent *event = [NSApp nextEventMatchingMask:NSEventMaskAny
					untilDate:[NSDate dateWithTimeIntervalSinceNow:kEventSliceSeconds]
					inMode:NSDefaultRunLoopMode
					dequeue:YES];
				if (event != nil) {
					[NSApp sendEvent:event];
				} else if (![window isKeyWindow] && [focus_deadline timeIntervalSinceNow] > 0) {
					[NSApp activateIgnoringOtherApps:YES];
				}
			}
		}

		BOOL committed = [view isCommitted];
		NSArray *rects = [[[view rects] copy] autorelease]; // 视图随窗口一起销毁，先拷出来
		[window orderOut:nil];
		[window close];
		[window release];
		if (veil != nil) {
			[veil orderOut:nil];
			[veil close];
			[veil release];
		}

		if (!committed) return JT_ANNOTATE_CANCELLED;
		jt_write_annotated_image(pasteboard, region_image, rects, scale);
		return JT_ANNOTATE_COMMITTED;
	}
}
