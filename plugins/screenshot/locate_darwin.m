//go:build darwin && cgo

#include "locate_darwin.h"

#import <AppKit/AppKit.h>
#include <math.h>
#include <stdlib.h>
#include <string.h>

// 定位选区时的比对参数：候选位置由框选起点算出，只剩 120Hz 轮询和取整带来的几像素误差。
enum {
	kSearchRadiusPx = 32,     // 在候选位置 ±32 像素内搜
	kSearchGrid = 24,         // 选区上取 24x24 个采样点
	kChannelTolerance = 24,   // 显示回采有舍入，逐通道留余量
	kMaxMismatchPermille = 20 // 采样点不匹配比例上限 2%
};

CGImageRef jt_image_cgimage(NSImage *image) {
	return [image CGImageForProposedRect:NULL context:nil hints:nil];
}

// 系统写进剪贴板的 PNG 带 dpi 元数据（Retina 上是 144dpi），NSImage 的 size 于是是点不是像素。
// 这里让图自己说「一个点等于几个像素」，不去猜屏幕的 backingScaleFactor——1x 外接屏同样成立。
// 取不到图或尺寸为零时返回 0，调用方据此跳过标注。
CGFloat jt_image_pixels_per_point(NSImage *image) {
	CGImageRef cg = jt_image_cgimage(image);
	if (cg == NULL) return 0.0;
	NSSize points = [image size];
	if (points.width < 1.0 || points.height < 1.0) return 1.0;
	return CGImageGetWidth(cg) / points.width;
}

NSPoint jt_hint_origin_top_px(NSPoint drag_start, NSPoint drag_end, NSRect screen_frame, NSSize region_px, CGFloat scale) {
	CGFloat anchor_x = (drag_start.x - NSMinX(screen_frame)) * scale;
	CGFloat anchor_top = (NSHeight(screen_frame) - (drag_start.y - NSMinY(screen_frame))) * scale;
	// 屏幕坐标 y 向上：往右拖则起点在左，往上拖则起点在下
	BOOL dragged_right = drag_end.x >= drag_start.x;
	BOOL dragged_up = drag_end.y >= drag_start.y;
	return NSMakePoint(dragged_right ? anchor_x : anchor_x - region_px.width,
		dragged_up ? anchor_top - region_px.height : anchor_top);
}

// 把图的一块（像素坐标，原点在左上角）画进统一格式的缓冲，方便逐字节比对。
// 两张图都转进同一种上下文，色彩转换一致，所以不需要关心通道顺序。返回的数据要 free()。
static unsigned char *jt_copy_pixels(NSImage *image, NSInteger x, NSInteger y_top, NSInteger width, NSInteger height, size_t *out_stride) {
	CGImageRef cg = jt_image_cgimage(image);
	if (cg == NULL || width <= 0 || height <= 0) return NULL;
	CGColorSpaceRef space = CGColorSpaceCreateDeviceRGB();
	CGContextRef ctx = CGBitmapContextCreate(NULL, (size_t)width, (size_t)height, 8, 0, space,
		(CGBitmapInfo)kCGImageAlphaPremultipliedFirst);
	CGColorSpaceRelease(space);
	if (ctx == NULL) return NULL;

	// 上下文坐标 y 向上，而 y_top 是从上往下数的，平移整张图让要的那块正好落在画布内
	CGFloat image_height = CGImageGetHeight(cg);
	CGContextTranslateCTM(ctx, -x, -(image_height - y_top - height));
	CGContextDrawImage(ctx, CGRectMake(0, 0, CGImageGetWidth(cg), image_height), cg);

	*out_stride = CGBitmapContextGetBytesPerRow(ctx);
	unsigned char *data = NULL;
	void *buffer = CGBitmapContextGetData(ctx);
	if (buffer != NULL) {
		data = malloc(*out_stride * (size_t)height);
		if (data != NULL) memcpy(data, buffer, *out_stride * (size_t)height);
	}
	CGContextRelease(ctx);
	return data;
}

BOOL jt_locate_region(NSImage *frozen, NSImage *region, NSPoint hint_top_px, NSPoint *out_top_px) {
	CGImageRef frozen_cg = jt_image_cgimage(frozen);
	CGImageRef region_cg = jt_image_cgimage(region);
	if (frozen_cg == NULL || region_cg == NULL) return NO;
	NSInteger frozen_w = (NSInteger)CGImageGetWidth(frozen_cg), frozen_h = (NSInteger)CGImageGetHeight(frozen_cg);
	NSInteger region_w = (NSInteger)CGImageGetWidth(region_cg), region_h = (NSInteger)CGImageGetHeight(region_cg);
	if (region_w <= 0 || region_h <= 0 || region_w > frozen_w || region_h > frozen_h) return NO;

	NSInteger hint_x = (NSInteger)llround(hint_top_px.x), hint_y = (NSInteger)llround(hint_top_px.y);
	NSInteger min_x = MAX(0, hint_x - kSearchRadiusPx), max_x = MIN(frozen_w - region_w, hint_x + kSearchRadiusPx);
	NSInteger min_y = MAX(0, hint_y - kSearchRadiusPx), max_y = MIN(frozen_h - region_h, hint_y + kSearchRadiusPx);
	if (min_x > max_x || min_y > max_y) return NO;

	size_t region_stride = 0, window_stride = 0;
	unsigned char *region_pixels = jt_copy_pixels(region, 0, 0, region_w, region_h, &region_stride);
	if (region_pixels == NULL) return NO;
	// 一次性拷进「所有候选位置 ∪ 选区大小」的外接框，比对时按偏移索引
	NSInteger window_w = (max_x - min_x) + region_w, window_h = (max_y - min_y) + region_h;
	unsigned char *window_pixels = jt_copy_pixels(frozen, min_x, min_y, window_w, window_h, &window_stride);
	if (window_pixels == NULL) {
		free(region_pixels);
		return NO;
	}

	NSInteger sample_x[kSearchGrid], sample_y[kSearchGrid];
	for (NSInteger i = 0; i < kSearchGrid; i++) {
		sample_x[i] = (NSInteger)((i + 0.5) * region_w / kSearchGrid);
		sample_y[i] = (NSInteger)((i + 0.5) * region_h / kSearchGrid);
	}

	NSInteger limit = kSearchGrid * kSearchGrid * kMaxMismatchPermille / 1000 + 1;
	NSInteger best_mismatch = kSearchGrid * kSearchGrid + 1, best_x = min_x, best_y = min_y;
	BOOL found_exact = NO;
	for (NSInteger y = min_y; y <= max_y && !found_exact; y++) {
		for (NSInteger x = min_x; x <= max_x && !found_exact; x++) {
			NSInteger mismatch = 0;
			for (NSInteger j = 0; j < kSearchGrid && mismatch <= limit; j++) {
				const unsigned char *region_row = region_pixels + (size_t)sample_y[j] * region_stride;
				const unsigned char *window_row = window_pixels + (size_t)(sample_y[j] + y - min_y) * window_stride;
				for (NSInteger i = 0; i < kSearchGrid; i++) {
					const unsigned char *a = region_row + (size_t)sample_x[i] * 4;
					const unsigned char *b = window_row + (size_t)(sample_x[i] + x - min_x) * 4;
					if (abs(a[0] - b[0]) > kChannelTolerance || abs(a[1] - b[1]) > kChannelTolerance ||
						abs(a[2] - b[2]) > kChannelTolerance) {
						if (++mismatch > limit) break;
					}
				}
			}
			if (mismatch < best_mismatch) {
				best_mismatch = mismatch;
				best_x = x;
				best_y = y;
			}
			if (best_mismatch == 0) found_exact = YES;
		}
	}
	free(region_pixels);
	free(window_pixels);
	if (best_mismatch > limit) return NO;
	*out_top_px = NSMakePoint(best_x, best_y);
	return YES;
}
