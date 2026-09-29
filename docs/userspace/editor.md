# Text editor

`edit FILE` is the native full-screen text editor. It is a normal command with
`GrantVFS` and `GrantTerminal`: file access crosses the caller's VFS session,
input arrives on standard input, and the display is ANSI output on standard
output. It has no framebuffer, keyboard, block-device or filesystem service
authority.

`FILE` resolves against the working directory like any other command argument.
An existing regular file opens for editing. A name that does not exist yet
opens an empty document marked "New file", and the first save creates the
file. A directory, a device or any other non-file is refused.

## Keys

- Arrows move one character or one line. Up and Down keep the column the
  cursor last moved to, so the cursor returns to it after a shorter line.
- Home and End move to the start and end of the line.
- PageUp and PageDown move one screen of lines, and the view scrolls with the
  cursor so it keeps its screen row.
- Printable characters, including multi-byte UTF-8, insert at the cursor.
  Enter inserts a newline (LF) and Tab inserts a tab byte.
- Backspace and Delete remove one character. At the start or end of a line
  they join it with its neighbour.
- F2 saves to the current path.
- F3 opens Save As with the current path filled in. Typing adds to the end of
  the name and Backspace removes its last character. A relative name resolves
  against the working directory. Saving onto another existing file asks
  first; saving onto a directory is refused.
- F10 quits. With unsaved changes it asks: `y` or Enter saves and quits, `n`
  quits without saving, Escape cancels.
- Escape cancels any dialog. No other key is reserved; Ctrl and Alt
  combinations are ignored.

The screen has three parts. The text area fills every row but the last two.
The status line shows the path, `[modified]` while the document differs from
the saved file, and the cursor's line and column. The bottom row shows the
last message, amber for an error, or the key summary. The editor starts at
80x24 until a COM1 terminal reports its geometry; framebuffer geometry comes
from the terminal session. Both are bounded to 256x128 cells.

## Memory bound and data structure

The document is a gap buffer in one fixed 1 MiB array. The text sits on both
sides of a gap; typing fills the gap at the cursor and deleting widens it.
Moving the gap copies only the bytes between its old and new place, so an
edit costs the distance the cursor travelled since the last edit, never a copy
of the whole document. There is no line index: the editor tracks the
cursor's line as it moves and scans only the cursor's line and the visible
lines to draw a frame.

The storage is part of the command's package-level state, which the loader
maps as demand-zero memory. A small file touches few of its pages. All of the
editor's fixed state, including screen cells and render buffers, is about
1.4 MiB, well inside the 32 MiB `normal` launch profile (ADR 0122).

A file larger than 1 MiB, or one that contains a NUL byte, is refused at open
with a message that suggests `gview`, which reads files of any size through a
fixed window. Loading reads through the 64 KiB bulk buffer, 64 KiB at a time.
A file that grows past the limit while it loads is refused too. Inserting into
a full document leaves it unchanged and shows an error. ADR 0131 records the
choice of structure and limit.

## Save safety

Every save goes through `utility.SaveFile` (ADR 0129). It writes the document
to `.<name>.~save` in the file's directory through the bulk buffer, closes it
and asks the VFS to replace the file with it. No step truncates or rewrites
the original, so a failed save leaves the file as it was. The editor reports
the failure on the message line and keeps running, and the document stays
marked modified:

| Cause | Message |
| --- | --- |
| read-only mount or system image | `Save failed: read-only filesystem` |
| full volume | `Save failed: no space left on device` |
| file held open by another session | `Save failed: the file is open elsewhere` |
| filesystem service gone | `Save failed: storage service unavailable` |
| filesystem without Replace | `Save failed: operation not supported` |

Save As switches the document to the new path only after the save succeeds.
Saving across filesystems works, because the temporary file lives beside the
target.

## Input safety

File bytes are data, never terminal output. Newline is the only line
separator. Each character takes one cell: tab shows as one space, other C0
controls, DEL and the C1 controls U+0080 to U+009F show as `?`, and invalid
UTF-8 bytes follow the terminal's one-cell replacement glyph. ESC and CSI
bytes in a file therefore cannot become terminal commands. The substitution
happens only on screen; a save writes the document's bytes exactly, including
tabs, CR bytes and invalid sequences. The cursor steps over an invalid byte
one byte at a time.

## Terminal recovery

The shell owns raw mode (ADR 0124). On F10 the editor resets colours, shows
the cursor and clears the screen before it exits. When the editor is stopped
with Ctrl-C, killed or crashes, the shell ends raw mode as soon as the job
ends, and the terminal service writes the same reset. Ctrl-C always reaches
the shell, which is why the editor cannot use it.

A refused file is reported on the editor's own screen until Enter, Escape or
F10, and the command then exits with status 1. A message written to standard
error just before exit would be cleared by that reset.

Validation builds write `EDIT_EXIT_OK` after a normal exit, preceded by
`EDIT_SAVE_OK` when at least one save succeeded.

## Limitations

- No undo, search, selection, clipboard or syntax highlighting.
- Tab is one cell wide; there are no tab stops.
- Enter inserts LF. A CRLF file keeps its CR bytes, which show as `?` at line
  ends.
- The Save As field edits only at its end.
- Horizontal scrolling follows the cursor; there is no line wrapping.
- Unsaved changes are lost when the editor is stopped with Ctrl-C or killed.
- A save interrupted by a crash can leave its `.<name>.~save` file; the next
  save of the same file removes it.
- Drawing a frame scans each visible line to its end, so a single line close
  to 1 MiB makes every key cost a full scan of it.
