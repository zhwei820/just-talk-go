#pragma once

#import <AppKit/AppKit.h>

// 标注窗要摆回选区原地，就得先知道系统截的那块在哪里——系统只给图不给矩形，
// 所以这里靠图像本身的匹配把位置找回来。包内使用。

CGImageRef jt_image_cgimage(NSImage *image);
CGFloat jt_image_pixels_per_point(NSImage *image);

// 框选起点一定是选区的一个真角（Shift 约束只移动对角），配上区域图尺寸就能算出左上角候选位置。
// 返回像素坐标，原点在图像左上角。screen_frame 是冻结图对应的屏幕。
NSPoint jt_hint_origin_top_px(NSPoint drag_start, NSPoint drag_end, NSRect screen_frame, NSSize region_px, CGFloat scale);

// 区域图是冻结图的一块精确裁切，在候选位置附近按稀疏采样点比对，命中就给出精确的左上角像素位置。
BOOL jt_locate_region(NSImage *frozen, NSImage *region, NSPoint hint_top_px, NSPoint *out_top_px);
