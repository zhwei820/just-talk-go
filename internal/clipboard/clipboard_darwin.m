#include "clipboard_darwin.h"

#import <AppKit/AppKit.h>
#include <stdlib.h>

int jt_clipboard_set(const char *text) {
	@autoreleasepool {
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		[pb clearContents];
		NSString *s = [NSString stringWithUTF8String:text == NULL ? "" : text];
		return [pb setString:s forType:NSPasteboardTypeString] ? 0 : -1;
	}
}

char *jt_clipboard_get(void) {
	@autoreleasepool {
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		NSString *s = [pb stringForType:NSPasteboardTypeString];
		if (s == nil) {
			return strdup("");
		}
		return strdup([s UTF8String]);
	}
}

void *jt_clipboard_snapshot(void) {
	@autoreleasepool {
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		NSArray<NSPasteboardItem *> *items = [pb pasteboardItems];
		if (items == nil) {
			items = @[];
		}

		NSMutableArray *snap = [NSMutableArray arrayWithCapacity:items.count];
		for (NSPasteboardItem *item in items) {
			NSMutableDictionary *types = [NSMutableDictionary dictionary];
			for (NSString *type in item.types) {
				NSData *data = [item dataForType:type];
				if (data != nil) {
					types[type] = data;
				}
			}
			[snap addObject:types];
		}

		// Retained copy — caller must free via jt_clipboard_restore
		return (void *)CFBridgingRetain(snap);
	}
}

int jt_clipboard_restore(void *handle) {
	if (handle == NULL) {
		return 0;
	}
	@autoreleasepool {
		NSArray *snap = (__bridge NSArray *)handle;

		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		[pb clearContents];

		if (snap.count == 0) {
			CFRelease((__bridge CFTypeRef)snap);
			return 0;
		}

		NSMutableArray *items = [NSMutableArray arrayWithCapacity:snap.count];
		for (NSDictionary *types in snap) {
			NSPasteboardItem *item = [[NSPasteboardItem alloc] init];
			for (NSString *type in types) {
				[item setData:types[type] forType:type];
			}
			[items addObject:item];
		}

		BOOL ok = [pb writeObjects:items];
		CFRelease((__bridge CFTypeRef)snap);
		return ok ? 0 : -1;
	}
}
