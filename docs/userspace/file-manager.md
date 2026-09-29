# File manager

`gfm` is the GoOS two-pane file manager, and the first full-screen program the
system runs ([ADR 0124](../adr/0124-foreground-raw-terminal-mode-and-tui-input.md)).
Start it from the shell:

```sh
gfm
```

Both panels open on the shell's working directory. The one with the keyboard
has bright frames, a highlighted title and a steel-blue selection bar; the
other one's frame is dimmer and its selection grey. There is no menu bar and
no panel label.

```text
┌─ /home ────────────────────────────┐┌─ /system ──────────────────────────┐
│ Name                     │    Size ││ Name                     │    Size │
│ ..                       │    <UP> ││ ..                       │    <UP> │
│ notes/                   │   <DIR> ││ bin/                     │   <DIR> │
│ report.txt               │   26417 ││ commands                 │    1893 │
│                          │         ││ motd                     │     120 │
├──────────────────────────┴─────────┤├──────────────────────────┴─────────┤
│ report.txt   26417  28 Sep 21:10   ││ ..                            <UP> │
└─ 18.3G free ────────── 2 entries ──┘└─ image ro ───────────── 3 entries ──┘
 /home/report.txt
F1 Help F2 Reread F3 View F4 Edit F5 Copy F6 Move F7 Mkdir F8 Delete F10 Quit
```

## Keys

| Key | Action |
| --- | --- |
| Up, Down | move the selection |
| PageUp, PageDown | move by one screen of entries |
| Home, End | first or last entry |
| Enter | open the selected directory (`..` goes up), view the selected file |
| Backspace | go to the parent directory, selecting the one left |
| Tab | switch panels |
| F1 | help; any key closes it |
| F2 | read both directories again and repaint the whole screen |
| F3 | view the selected file with `gview` |
| F4 | edit the selected file with `edit` |
| F5 | copy the selected file into the other panel's directory |
| F6 | move or rename: edit the offered target path |
| F7 | create a directory in the active panel |
| F8 | delete the selected file or empty directory |
| F10 | quit |
| Escape | cancel a dialog, clear the status line |

F9, F11 and F12 do nothing. Ctrl-C stops `gfm` like any foreground command,
and a viewer or editor it started with it.

## Viewer and editor

F3 and F4 run `gview` or `edit` on the selected regular file, with the active
panel's directory as working directory; on a directory or `..` they say
`Select a file to open with ...`. `gfm` asks PID 1 to start the program
through its launch channel (ADR 0130) and waits. The program reads the keys
and draws on the terminal until it exits; `gfm` reads nothing meanwhile. When
it returns, `gfm` reads both directories again, keeps the same entry selected
when it still exists, and repaints the whole screen. A program that cannot be
started, or ends with a nonzero status, is a message on the status line:
`Cannot start edit: not found`, `gview ended with status 1`.

## Operations

**Copy** reads the source and writes the target through a 64 KiB shared buffer,
one exchange at a time, so a file of any size costs the same memory. An existing
target is replaced only after a yes to `Overwrite "name"?`. A directory is not
copied: recursive copy is not implemented. Copying a file onto itself is
refused.

**Move** offers the other panel's directory joined with the name. Enter moves
there; editing the path renames, and a path naming an existing directory moves
into it. A move is a VFS rename, so it works within one filesystem only; across
two the VFS answers `unsupported` and `gfm` says `Cross-filesystem move is not
supported`. It never copies and deletes.

**Mkdir** takes one name: UTF-8 is kept as typed, but `/`, `.` and `..` are
refused.

**Delete** asks `Delete "name"?` first (Enter or `y` for yes, Escape or `n`
for no). It removes a file or an empty directory; a directory with contents is
reported as not empty, never removed recursively.

Every failure is a message in amber on the status line, worded from the VFS
status (`read-only filesystem`, `no space left on device`, `no such file or
directory`, ...). Nothing that fails ends the program, and a failed operation
leaves its source as it was. A panel whose directory vanished climbs to the
nearest parent that can be listed.

## Display

The QEMU window and the terminal on COM1 are separate consoles with a shell each
(ADR 0125), and `gfm` fills the one you start it on. In the QEMU window it
takes the framebuffer's size from `COLUMNS` and `LINES`: 106x33 cells at
1280x800 with the 12x24 font. On COM1 it asks your terminal with a cursor
position report (`ESC[6n`) at start, on F2, and after every second without a
key, so resizing the window re-lays the panels within a second. Until the
terminal answers, and if it never does, `gfm` uses 80x24. Below 80x24 it shows
`terminal too small (need at least 80x24); F10 quits` until the screen grows
again. Each panel lists at most 1024 entries holding
64 KiB of names; a larger directory is shown with `+ entries, list truncated`
in amber on its bottom frame, never cut silently. Names are sorted with `..`
first, directories before files, ASCII letters compared case-insensitively and
the rest by bytes. Long names are cut with `~`; control bytes in a name are
shown as `?`, so a name cannot send escape sequences to the terminal.

Sizes up to 9999999 are exact bytes; larger ones are scaled (`9.5M`, `674M`,
`1.2G`). Modification times are UTC: `28 Sep 21:10` in the current year,
`28 Sep  2025` in another, `-` when the filesystem kept none. A panel at least
45 columns wide inside its frame, which a 106-column framebuffer gives, has a
`Modified` column; a narrower one shows the selected entry's time on the
information line instead, so 80x24 keeps its name width.

Each panel's bottom frame describes the filesystem its directory is on:
`fat32 18.3G free / 32.0G` when that fits, `18.3G free` when only that does,
`image ro` for a read-only one. `gfm` asks the VFS each time it lists a
directory (a directory change, F2, an operation, a return from the viewer or
editor) and never while the selection moves.

Colours are ANSI roles, so the framebuffer and a host terminal on COM1 show the
same screen in their own palettes: directories bright yellow, files light
grey, frames cyan, the selection white on blue, the key bar's labels white on
cyan and its numbers yellow, errors bright yellow. After the first frame only
changed rows are sent: moving the selection redraws a few rows. Output other
processes write while `gfm` is on the screen, such as a background job's
report, lands on its display; F2 repaints.

## Structure

- `userspace/bin/gfm` is the command: its manifest asks for `GrantVFS` and
  `GrantTerminal`, and it connects the manager to standard input, standard
  output and the VFS session.
- `userspace/filemanager` is the manager itself — panels, keys, dialogs and
  rendering — reaching files only through a `FileSystem` interface, so all of
  it is tested on the host against a model filesystem and a model terminal.
- `userspace/terminal` provides the output builder and the key parser.
- `userspace/utility` provides the filesystem calls that report failure
  instead of ending the command (`ReadDir`, `Stat`, `Mkdir`, `Rename`,
  `Unlink`, `CopyFile`, `FilesystemInfo`), `TerminalSize` and `Launch`.
- The manifest also asks for `GrantLaunch`, which gives it the launch channel
  F3 and F4 use.

## Not in 0.1.7

No embedded shell or command line, mouse, archive browsing,
plugins, recursive copy or delete, cross-filesystem move, permission or owner
editing, symlink handling, multiple selection, sort menu or locale collation;
16 ANSI colours only.
