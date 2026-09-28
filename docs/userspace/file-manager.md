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
┌─ /tmp ─────────────────────────────┐┌─ /system ──────────────────────────┐
│ Name                     │    Size ││ Name                     │    Size │
│ ..                       │    <UP> ││ ..                       │    <UP> │
│ notes/                   │   <DIR> ││ bin/                     │   <DIR> │
│ report.txt               │   26417 ││ commands                 │    1893 │
│                          │         ││ motd                     │     120 │
├──────────────────────────┴─────────┤├──────────────────────────┴─────────┤
│ notes/                       <DIR> ││ ..                            <UP> │
└─────────────────────── 2 entries ──┘└─────────────────────── 3 entries ──┘
 /tmp/notes
F1 Help   F2 Reread  F5 Copy   F6 Move   F7 Mkdir  F8 Delete  F10 Quit
```

## Keys

| Key | Action |
| --- | --- |
| Up, Down | move the selection |
| PageUp, PageDown | move by one screen of entries |
| Home, End | first or last entry |
| Enter | open the selected directory (`..` goes up) |
| Backspace | go to the parent directory, selecting the one left |
| Tab | switch panels |
| F1 | help; any key closes it |
| F2 | read both directories again and repaint the whole screen |
| F5 | copy the selected file into the other panel's directory |
| F6 | move or rename: edit the offered target path |
| F7 | create a directory in the active panel |
| F8 | delete the selected file or empty directory |
| F10 | quit |
| Escape | cancel a dialog, clear the status line |

F3 and F4 report that no viewer or editor exists in this release; F9, F11 and
F12 do nothing. Ctrl-C stops `gfm` like any foreground command.

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

The same screen is drawn on the framebuffer and on the terminal attached to
COM1, so `gfm` lays itself out on a size both can show. The framebuffer's size
comes from the shell in `COLUMNS` and `LINES` (106x33 cells at 1280x800 with
the 12x24 font). A host terminal on COM1 is asked for its size with a cursor
position report (`ESC[6n`) when `gfm` starts, on F2, and every second while
nothing is typed, so resizing that window re-lays the panels within about a
second. With both, the screen is the smaller of the two in each dimension: a
host terminal smaller than the framebuffer shrinks the panels on both, a larger
one cannot grow them past the framebuffer. On a serial-only console the host
terminal's size is used as it is; if it does not answer, 80x24. Below 80x24
`gfm` shows `terminal too small (need at least 80x24); F10 quits` until the
screen grows again. Each panel lists at most 1024 entries holding
64 KiB of names; a larger directory is shown with `+ entries, list truncated`
in amber on its bottom frame, never cut silently. Names are sorted with `..`
first, directories before files, ASCII letters compared case-insensitively and
the rest by bytes. Long names are cut with `~`; control bytes in a name are
shown as `?`, so a name cannot send escape sequences to the terminal.

Sizes up to 9999999 are exact bytes; larger ones are scaled (`9.5M`, `674M`,
`1.2G`). There is no date column and no free-space figure: the VFS reports
neither.

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
  `Unlink`, `CopyFile`) and `TerminalSize`.

## Not in 0.1.6

No embedded shell or command line, mouse, viewer, editor, archive browsing,
plugins, recursive copy or delete, cross-filesystem move, permission or owner
editing, symlink handling, multiple selection, sort menu or locale collation;
16 ANSI colours only.
