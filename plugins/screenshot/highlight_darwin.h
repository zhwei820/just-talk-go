#pragma once

enum {
	JT_SCREENSHOT_KEY_FULL = 20,   // kVK_ANSI_3
	JT_SCREENSHOT_KEY_REGION = 21, // kVK_ANSI_4
};

enum {
	JT_SCREENSHOT_UI_DONE = 0,
	JT_SCREENSHOT_UI_FAILED = 1,
	JT_SCREENSHOT_UI_CANCELLED = 2,
};

int jt_post_screenshot_shortcut(int key);
int jt_screenshot_ui_run(double timeout_seconds);
