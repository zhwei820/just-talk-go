#ifndef JUST_TALK_CLIPBOARD_DARWIN_H
#define JUST_TALK_CLIPBOARD_DARWIN_H

int jt_clipboard_set(const char *text);
char *jt_clipboard_get(void);
void *jt_clipboard_snapshot(void);
int jt_clipboard_restore(void *handle);

#endif
