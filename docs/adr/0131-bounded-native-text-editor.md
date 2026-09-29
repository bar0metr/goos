# ADR 0131: Bounded native text editor

## Status

Accepted.

## Context

GoOS 0.1.7 adds `edit`, a full-screen editor for text files. It runs as an
ordinary command under the `normal` launch profile (ADR 0122): 8192 pages,
32 MiB. Every other long-running native program keeps its memory in fixed
package-level storage and resets its heap after each key, and the editor has
to follow the same rule. It cannot size a heap object from the file, since a
large file would then end the process with `page quota exhausted` in the
middle of an edit.

`gview` already views files of any size through a 4 KiB window. An
editor has to hold the whole document, because a save writes every byte of it
through the safe replacement path (ADR 0129) and never patches the original
in place.

Three document structures were considered:

- a gap buffer: one array, the text on both sides of a gap at the cursor;
- a piece table: the original file plus an append buffer and a list of pieces
  describing the current text;
- an array of lines, each with its own storage.

The piece table and the line array need a second bounded structure, the piece
list or the line table, with its own limit and its own out-of-space case. The
line array also turns every line split and join into storage management. A
gap buffer needs one array and one limit.

## Decision

The document is a gap buffer in a fixed 1 MiB array inside the editor state.
The command declares that state at package level, so the loader maps it as
demand-zero memory and a small file touches few of its pages.

- Insertion and deletion happen at the gap. Moving the gap copies the bytes
  between its old and its new place, so an edit costs the distance from the
  previous edit and no keystroke copies the whole document.
- There is no line index. The editor tracks the cursor's line number as it
  moves and edits, finds line starts by scanning for newline bytes, and a
  frame scans only the visible lines.
- A file larger than 1 MiB, or one containing a NUL byte, is refused at open
  with a message that points to `gview`. The file loads through the 64 KiB
  bulk buffer in chunks of at most 64 KiB, and a file that grows past the
  limit during the load is refused as well.
- Text and gap share the 1 MiB, so an insertion that would exceed it is
  refused with a message and the document stays as it was.
- A save hands the document to `utility.SaveFile` as a `safesave.Source` that
  copies it out 64 KiB at a time. The editor keeps no second copy.

1 MiB keeps each costly operation small on a TCG-emulated machine. The worst
gap move, the worst scan of one line and the load each touch at most 1 MiB,
and a save is at most sixteen bulk writes. Text files on GoOS are
configuration, logs and short notes, far below this size, and `gview` covers
larger files read-only. The editor's fixed state, including screen cells and
render buffers, comes to about 1.4 MiB against the 32 MiB profile.

`userspace/tui` gains `RoleWarning`, amber on the ground colour as `gfm` shows
its warnings, for save errors, and a `Screen.Cell` accessor for tests that read
frames back.

## Consequences

- An oversized or binary file fails at open with a message. Memory use cannot
  grow from the file size during editing.
- Keystrokes near each other are cheap. Alternating edits at opposite ends of
  a 1 MiB document copy up to 1 MiB per edit.
- A file with one very long line costs a scan of that line per frame, because
  there is no line index.
- The limit is a compile-time constant. Raising it costs only address space
  and the per-operation bounds above, and needs no change to the structure.
- No undo history exists. Adding one needs its own bounded storage and its
  own limit.
