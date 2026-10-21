# Just Talk

[中文](README.md)

Just Talk is a desktop voice input tool. It records audio with a global hotkey, sends it to streaming ASR, and then copies the recognized text to the clipboard or submits it directly into the focused input field.

It is built for people who want to type less and speak more while coding, chatting, writing notes, or working with long text.

## Screenshot

![Just Talk TUI](docs/screenshot-tui.png)

## Features

- Global hotkey recording with `toggle` and `hold` modes.
- Voice hotkeys are limited to keys suitable for global shortcuts: modifiers, function keys, Tab, CapsLock, arrow/navigation keys, and similar non-text keys. Letters, digits, punctuation, Space, and other text-producing keys are rejected.
- Streaming ASR: Microsoft Azure Speech by default, with Doubao (Volcengine) as an alternative.
- Clipboard copy and automatic text submission.
- Always-on-top recording status overlay for Wayland, X11, and macOS.
- TUI configuration for hotkeys, mode, auto-submit, stop delay, hotwords, and related settings.
- ASR hotwords for project names, people names, English terms, and domain-specific vocabulary.
- Usage statistics for total sessions, total recognized characters, average speed, and recent speed.
- Region screenshot: `Ctrl+Command+A` by default, select with the mouse and the image goes straight to the clipboard.
- Stay active (macOS): keeps the system from going idle during set hours, so apps like Slack do not show you as away.

## Platform Status

The current development focus is Linux and macOS desktop support:

| Platform | Status | Notes |
| --- | --- | --- |
| Linux Wayland | Supported | Works with Sway / wlroots; hotkeys use evdev and require input permissions |
| Linux X11 | Supported | Uses native X11 global hotkeys |
| macOS | Supported | Global hotkeys use CGEventTap, recording uses CoreAudio, clipboard uses NSPasteboard, and overlay uses AppKit NSPanel |
| Windows | Not implemented | Not supported yet |

## Build

Just Talk uses native platform APIs, so builds require cgo.

Linux build dependencies:

```bash
# Arch Linux
sudo pacman -S --needed go gcc libx11 libxtst libxext wayland

# Debian / Ubuntu
sudo apt install golang-go build-essential libx11-dev libxtst-dev libxext-dev libxinerama-dev libwayland-dev
```

macOS build dependencies:

```bash
# Apple Command Line Tools provide clang and the macOS SDK. Full Xcode is not required.
xcode-select --install
```

Build for the current platform:

```bash
CGO_ENABLED=1 go build -o build/just-talk ./cmd/just-talk
```

Install to `~/.local/bin/just-talk`:

```bash
build/just-talk --install
# or
make install
```

macOS must be built on macOS. The project does not provide a non-cgo build.

## Usage

Start the TUI:

```bash
just-talk
```

Run without the TUI:

```bash
just-talk --no-tui
```

Force a backend:

```bash
just-talk --backend wayland
just-talk --backend x11
```

## Configuration

Default config path:

```text
~/.config/just-talk/config.toml
```

Recommended hotkey config:

```toml
[voice]
mode = "toggle"
push_to_talk = "Alt+Super"
```

`Alt+Super` with `toggle` mode is recommended. Press once to start recording, then press again to stop. This avoids hold-mode key conflicts with desktop environments or focused input fields.

Voice hotkeys only support keys suitable for global shortcuts:

- Supported: modifier-only combinations, such as `Alt+Super` and `Ctrl+Alt+Shift`.
- Supported: function keys `F1` through `F24`, such as `F9` and `Alt+F8`.
- Supported: non-text control and navigation keys, such as `Tab`, `Enter`, `Escape`, `Backspace`, `CapsLock`, `Up`, `Down`, `Left`, `Right`, `Home`, `End`, `PageUp`, `PageDown`, `Insert`, and `Delete`.
- Not supported: letters, digits, punctuation, Space, numpad digits, and numpad symbols that can enter text, such as `Alt+G`, `G`, `Alt+1`, and `Alt+Space`.

ASR configuration (Azure by default; create a "Speech service" resource in the Azure portal and copy the key and region from its "Keys and Endpoint" page):

```toml
[voice]
provider = "azure"
azure_key = "<key>"
azure_region = "eastasia"
```

To use Volcengine, set `provider = "volcengine"` and fill in `app_key` and `access_key`.

Hotword example:

```toml
[voice]
hotwords = ["Wayland", "Sway", "wl-copy", "wtype", "just-talk-go"]
```

macOS hotkey example:

```toml
[voice]
# Option is Alt; Command/Cmd is Super.
push_to_talk = "Option+Command"
```

Region screenshot (the selected image is copied to the clipboard; Esc cancels):

```toml
[screenshot]
enabled = true
hotkey = "Ctrl+Cmd+A"
```

- macOS triggers the system shortcut `⌃⇧⌘4` (copy picture of selected area), so the terminal does not need Screen Recording permission. When the hotkey is pressed it first grabs the main display with `⌃⇧⌘3` and covers the screen with that still image, so you select from a frozen screen (you will hear the shutter sound twice); secondary displays are not frozen. Freezing or capturing stops working if these shortcuts are changed or disabled under System Settings > Keyboard > Keyboard Shortcuts > Screenshots. A red crosshair is drawn over the cursor until you press the mouse; Esc or 60 seconds without finishing counts as cancelled and the previous clipboard is restored. The default hotkey matches the macOS WeChat screenshot shortcut and both will fire, so change one of them. When the selection finishes you annotate in place: everything outside the region is dimmed, the cursor becomes a crosshair inside it, and dragging draws red rectangles (5 px wide, several allowed) right where they appear on screen. `Cmd+Z` or Backspace undoes the last one, `Enter` or double click copies, `Esc` or right click cancels; pressing `Enter` without drawing anything copies the original image. Clicks outside the region do not reach the application behind it while annotating.
- Linux X11 needs `maim`; Wayland needs `slurp` and `grim` and only works on wlroots compositors such as Sway / Hyprland. GNOME / KDE Wayland are not supported yet.

Stay active (macOS only, off by default):

```toml
[presence]
enabled = true
start = "09:00"
end = "20:00"   # earlier than start means the window spans midnight
```

- Within the window, after 4 minutes without keyboard or mouse input it posts an in-place mouse event (the cursor does not move) and keeps the display awake; outside the window it does nothing.
- Uses the Accessibility permission the terminal already has; does not work with the lid closed or the screen locked.

## Changelog

See [CHANGELOG.md](CHANGELOG.md).

## Maintenance And Contributions

Just Talk is maintained by `whoamihappyhacking`.

This project does not accept pull requests. Issues are welcome for bug reports, usage feedback, and feature discussion.

## License

Just Talk is licensed under the GNU General Public License v3.0.


