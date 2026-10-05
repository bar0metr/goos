# ADR 0186: chmod and utimensat through a VFS metadata operation

## Status

Accepted.

## Context

7-Zip 24.09 restores each extracted file's mode and modification time with
`fchmodat` and `utimensat`, and counts a refusal as an error: `x` printed
`ERROR: Cannot set ...` for every file and exited 2. BusyBox `touch` failed
the same way on an existing file. The VFS had no request that changed a
file without writing it, so `chmod`, `fchmod`, `fchmodat`, and `utimensat`
returned `ENOSYS`.

GoOS filesystems keep less than a Linux inode. FAT has one permission bit,
the read-only attribute, and one time that programs set, the last write.
ramfs keeps a modification time and now a read-only flag beside it.

## Decision

The VFS protocol gains `OperationSetMetadata`. A request names a file by path
or by an open handle and carries any of three changes: a modification time,
read-only on, read-only off. The kernel VFS carries the flags in the request's
rights byte and the time in its offset (seconds) and size (nanoseconds); the
filesystem protocol carries the flags in its parent field. A backend without
the operation answers `StatusUnsupported`, and a protected or pinned path is
`StatusPermissionDenied`, as for every other mutation.

ramfs refuses `Write` and `Truncate` on a read-only file with
`StatusPermissionDenied` and reports the flag in its status, as FAT32 already
did for `ATTR_READ_ONLY`. FAT32 writes the time into the entry's write date
and time and sets or clears `ATTR_READ_ONLY` on files. Neither filesystem has a read-only directory, and both refuse the
change there with `StatusIsDirectory`.

The Linux calls map onto the operation:

| Call | Change |
| --- | --- |
| `chmod`, `fchmod`, `fchmodat` | read-only when the mode has none of the 0222 bits, writable otherwise; other mode bits are accepted and not kept |
| `utimensat` | the modification time, `UTIME_NOW` from the realtime clock, `UTIME_OMIT` nothing; the access time is accepted and not kept |

A directory's mode change succeeds and changes nothing, as on a FAT volume
Linux mounts. A device name or a stream keeps no metadata, and the call
succeeds there. A change with nothing left in it, such as an access time
alone, still checks that the path exists. `utimensat` with a null path acts
on the descriptor, as `futimens` does; a flag other than
`AT_SYMLINK_NOFOLLOW` or `AT_EMPTY_PATH` is `EINVAL`, and a nanosecond field
out of range that is neither `UTIME_NOW` nor `UTIME_OMIT` is `EINVAL`.

## Consequences

7-Zip extracts with exit status 0, and BusyBox `touch -d` and `chmod` on an
existing file show in `stat`; the BusyBox boot test checks both. After
`chmod 0444` a write to the file fails with `EACCES`. A mode of 0700 and one of 0644 read back the same,
since the filesystems keep no owner, group, or execute bits. The access time
stays what `stat` already reported: the modification time.
