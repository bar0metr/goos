# ADR 0143: Linux directory listing

## Status

Accepted.

## Context

Linux programs list a directory with `getdents64`, which fills a buffer with
variable-length `linux_dirent64` records and resumes where the last call
stopped. The VFS lists a directory by path, one entry per request, and returns
a cursor for the next. A directory descriptor already keeps its canonical path
(ADR 0141), and a directory entry already carries its node identity
(ADR 0142).

## Decision

`getdents64` runs one VFS exchange per entry and writes each record to the
caller's buffer as it arrives.

| Field | Value |
| --- | --- |
| `d_ino` | the VFS node identity, the value `st_ino` reports for the same file; 1 when the node has none |
| `d_off` | the VFS cursor after this entry |
| `d_reclen` | 19 header bytes, the name, its terminator, padded to 8 |
| `d_type` | `DT_REG` or `DT_DIR` |
| `d_name` | the name, NUL-terminated |

The record layout is written out by hand and encoded in a pure function. A
record is delivered whole or not at all. One that does not fit ends the call:
`EINVAL` when it would be the first, the bytes so far otherwise, and its entry
stays unread for the next call.

The listing position is the VFS cursor. It lives in the directory's open-file
description, so duplicated descriptors share it, `lseek(fd, 0, SEEK_SET)`
rewinds, and `lseek` to a `d_off` resumes. A call past the last entry returns
zero.

A name of more than 152 bytes does not fit a VFS reply message. The VFS says
so, and the bridge asks for that entry again with the process's I/O memory
object attached for the name. A directory whose own path exceeds a request
message sends that path in the same object.

One call runs at most 128 exchanges. A listing shorter than the buffer could
hold is a valid answer, and the next call continues it. An entry that fails
after others were delivered ends the call with those; the failure reaches the
caller on its next call.

GoOS directories hold no `.` and `..` entries, and the listing invents none.
POSIX lets `readdir` omit them.

| Condition | Result |
| --- | --- |
| descriptor not open | `EBADF` |
| descriptor not a directory | `ENOTDIR` |
| buffer too small for the next record | `EINVAL` |
| unwritable buffer | `EFAULT` |

## Consequences

A directory of N entries costs N exchanges and one more to find the end. The
VFS resolves the path on each; its resolution cache absorbs most of that.
Listing order is the filesystem's. A program that expects `.` and `..` in a
listing does not find them; one that skips them, as most do, sees no
difference. The VFS gives one answer at the end of a directory and for a
directory that no longer exists, so a directory removed while open lists as
empty where Linux reports `ENOENT`.
