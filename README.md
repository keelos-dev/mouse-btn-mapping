# mouse-btn-mapping

A tiny Windows GUI that remaps Logitech G502 buttons **directly in the mouse's onboard memory** — no G HUB, no LGS, nothing running in the background afterwards.

Typical use: map spare buttons to F13 / F14 / F15 and handle them in AutoHotkey.

一个很小的 Windows 工具，把罗技 G502 的按键映射直接写进鼠标板载内存。写完不需要 G HUB / LGS，也没有任何常驻程序。典型用法：把多余的键映射成 F13/F14/F15，再交给 AutoHotkey 处理。

## Features

- Pick an action per button: F13–F15, any key with Ctrl/Shift/Alt/Win modifiers, mouse buttons, DPI / profile functions, or disabled
- Edits all onboard profiles; changes take effect immediately
- Generates an AutoHotkey v2 template for the keys you mapped
- Safety: the original config is backed up on first connect, every write is preceded by a backup and verified by read-back, and there are "restore original backup" and "factory reset (from mouse ROM)" buttons
- Single exe, pure Go (no cgo), UI via Microsoft Edge WebView2

## Supported devices

Tested on **G502 Proteus Spectrum** (USB PID `C332`).

It talks HID++ 2.0 feature `0x8100` (Onboard Profiles), so other Logitech G mice with onboard memory will probably work too. Button names are only labelled for the 11-button G502 layout; other mice show "Button N". Reports welcome.

## Usage

1. Quit G HUB / Logitech Gaming Software (they switch the mouse to host mode, which disables onboard profiles).
2. Run `g502map.exe`.
3. Choose a profile tab, set actions, click **写入鼠标** (Write to mouse).

Backups live in `%APPDATA%\g502map\`.

Requires the WebView2 Runtime (preinstalled on Windows 10/11 with Edge).

## Build

```
go build -ldflags "-H windowsgui -s -w" -o g502map.exe .
```

## How it works

- HID access: Win32 `hid.dll` / overlapped `ReadFile`/`WriteFile` on the vendor collection (usage page `0xFF00`, usage `0x0002`, 20-byte long reports).
- Onboard profiles: 256-byte sectors; sector 0 is the profile directory, user profiles at 1..n, ROM defaults at `0x0101+`. Button bindings are 4 bytes each starting at offset `0x20` (`80 01 hi lo` mouse button, `80 02 mod key` keyboard, `90 xx ff ff` special). Each sector ends with a CRC-CCITT (poly `0x1021`, init `0xFFFF`).
- Protocol details cross-checked against [libratbag](https://github.com/libratbag/libratbag).

Not affiliated with Logitech.

## License

MIT
