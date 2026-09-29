# Text viewer

`gview FILE` is the native read-only full-screen text viewer. It is a normal
command with `GrantVFS` and `GrantTerminal`: file access crosses the caller's
VFS session, input arrives on standard input, and the display is ANSI output on
standard output. It has no framebuffer, keyboard, block-device or filesystem
service authority.

## Navigation

- Up and Down move one logical line.
- PageUp and PageDown move one visible page.
- Home moves to the start of the file; End positions the final logical lines.
- Left and Right scroll eight terminal columns.
- F2 redraws the screen and re-queries a serial terminal's size.
- Escape and F10 exit.

The status row contains the path, byte size, first visible byte offset and an
offset percentage. Long paths are truncated before the numeric fields. The
viewer starts at 80x24 until a COM1 terminal reports its geometry; framebuffer
geometry comes from the terminal session. Both are bounded to 256x128 cells.

## Bounded file access

The model reads through a fixed 4 KiB cache. The command implements those
random reads as VFS seek plus bulk read through the existing 64 KiB shared
transfer object. It does not allocate storage from file size, retain the whole
file or build an unbounded global line index. Home, End and reverse navigation
scan locally through that fixed window.

Screen cells, dirty rows and visible text storage are fixed caller-owned arrays.
Rendering uses `userspace/tui`; changed steady-state frames allocate nothing.
Long logical lines are clipped horizontally and cannot write outside the screen
or into the terminal's scrolling bottom-right cell.

## Input safety

File bytes are data, never terminal output. Newline is the only line separator,
tab is displayed as a space, and other C0/DEL controls are replaced by `?`
before cells are built. Invalid UTF-8 follows the terminal's bounded replacement
glyph behavior. This prevents ESC and CSI bytes in a file from becoming terminal
commands.

The first 4 KiB is classified as binary when it contains NUL or more than one
eighth non-whitespace control bytes. `gview` then refuses text rendering with a
clear message. Hex display is not implemented.
