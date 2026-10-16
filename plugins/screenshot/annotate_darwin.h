#pragma once

#import <AppKit/AppKit.h>

enum {
	JT_ANNOTATE_COMMITTED = 0, // 结果图已在剪贴板（画过框就是带框的，没画就是原图）
	JT_ANNOTATE_CANCELLED = 1, // 用户取消，调用方负责把剪贴板恢复成截图前的内容
	JT_ANNOTATE_SKIPPED = 2,   // 剪贴板上取不到图，没进标注，调用方保持框选结果
};

// 冻结屏信息：标注要把窗口放回选区原来的位置，靠的就是这几个量。
typedef struct {
	NSImage *image;     // 冻结的整屏图，可为 nil（那就退回居中浮窗）
	NSScreen *screen;   // 它对应的屏幕
	NSPoint drag_start; // 框选时鼠标按下的点（屏幕坐标）——选区的一个真角
	NSPoint drag_end;   // 框选时鼠标抬起的点，只用来判断往哪个方向拖
} jt_annotate_context_t;

// 让用户在框选出来的那块位置上画红框：Enter / 双击复制，Esc / 右键取消，⌘Z 撤销。
// 阻塞到用户做出选择为止。
int jt_annotate_capture(NSPasteboard *pasteboard, jt_annotate_context_t context);

// 把 rects（选区局部点坐标）用红色、5 个图像像素的线宽描进图里，导出同像素尺寸的新图。
// 单独暴露是为了能脱离 UI 直接验证线宽。
NSImage *jt_composite_rects(NSImage *image, NSArray *rects, CGFloat image_scale);
