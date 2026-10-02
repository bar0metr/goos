# ADR 0141: Linux file calls on the VFS bridge

## Status

Accepted. The bulk-data buffer ownership described below is superseded by
ADR 0177: each Linux thread has its own 64 KiB file-I/O object.

## Context

ADR 0138 defines one request and one reply on a process's VFS session, and
ADR 0140 turns a Linux path into a canonical one. Linux file calls need more
than one exchange each, a position per open file, a way to move more bytes
than a message holds, and descriptors for directories, which the VFS does not
open. musl also reaches the kernel through legacy call numbers where x86_64
has them.

## Decision

**Staged calls.** A call that needs several exchanges numbers its stages. The
continuation keeps the stage, the resolved paths, one note set when the call
begins, and one value passed from a stage to the next. The handler runs again
after every wake and reads its progress from the continuation alone. A stage
sends its request once: the continuation is marked issued before the message
enters the queue.

**Open.** `open` and `openat` stat the path, then create or truncate the file
when the flags ask, then open it:

| Flags and target | Result |
| --- | --- |
| missing, no `O_CREAT` | `ENOENT` |
| missing, `O_CREAT` | create, open |
| existing, `O_CREAT\|O_EXCL` | `EEXIST` |
| regular file, `O_TRUNC` | truncate, open |
| regular file, `O_DIRECTORY` or a trailing slash | `ENOTDIR` |
| directory, read-only | directory descriptor |
| directory with write access, `O_CREAT`, or `O_TRUNC` | `EISDIR` |

The accepted flags are the access mode, `O_CREAT`, `O_EXCL`, `O_TRUNC`,
`O_APPEND`, `O_DIRECTORY`, `O_CLOEXEC`, and three that change nothing here:
`O_LARGEFILE`, `O_NOCTTY`, and `O_NOFOLLOW`, since no GoOS file is a terminal
or a symbolic link. `O_DIRECTORY|O_CREAT` is `EINVAL`. Every other flag is
`EOPNOTSUPP`. The mode argument of `open` and `mkdir` is ignored: GoOS has no
permission bits.

**Directory descriptors.** The VFS lists and stats directories by path, so a
directory descriptor keeps the canonical path it was opened by. All Linux
processes share 32 such records; a 33rd open is `ENFILE`. A relative path in
an `*at` call resolves from the path of its `dirfd`.

**File position.** The VFS keeps the position of each open file. The kernel
mirrors it in the open-file description, which duplicated descriptors share.
`lseek` sends the new position to the VFS. `SEEK_END` first asks for the file
length.

**Stat by handle.** VFS protocol version 12 lets a stat with a handle and no
path describe that open file. `lseek(SEEK_END)` and `O_APPEND` use it, and it
stays correct after the file is renamed.

**Bulk data.** File bytes move through one 64 KiB memory object per Linux
process, created at the first file read or write and charged to the process.
A read lends it to the VFS to fill, a write to the VFS to read, each time
through a second handle with only the rights that request needs. One call
transfers at most 64 KiB; a longer request is a short read or write. `writev`
on a file gathers its elements into the object and is one write.

**Append.** A write through an `O_APPEND` descriptor is three exchanges: the
file length, a seek to it, the write. Two processes appending to one file can
interleave between those steps.

**Names.** `mkdir`, `unlink`, `rmdir`, and `rename` have their `*at` forms and
the legacy numbers musl uses. `unlink` and `rmdir` stat the name first, so
each refuses the other's kind with `EISDIR` or `ENOTDIR`. `rename` moves a
regular file with the VFS replace operation, which removes an existing target
in the same step, and a directory with the VFS rename. `renameat2` accepts no
flags.

**Known differences from Linux.**

- Removing or replacing a file that any session holds open is `EBUSY`. FAT32
  cannot keep an unlinked file alive.
- A directory moves only onto a free name. Onto an existing directory, even
  an empty one, it is `EEXIST`.
- A refused write is `EROFS`. That also covers a program image pinned while
  it runs, where Linux reports `ETXTBSY`.
- A rename across mounts is `EXDEV`, as on Linux; every GoOS mount is its own
  filesystem.

## Consequences

ADR 0142 adds the stat calls on the stat exchange and ADR 0143 `getdents64`
on the directory records. `readv` reads a file
through the same memory object: one read, delivered across the vector's
elements in order. `ftruncate` and `fsync` are not implemented and return
`ENOSYS`.
