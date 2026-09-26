# ADR 0110: Native name and path limits

## Status

Accepted; amended by ADR 0114, which raises the native component bound to
1024 bytes so every legal VFAT UTF-16 name has a native representation. The
FAT32 printable-ASCII rule is replaced by UTF-8 names in ADR 0112. Replaces the name and path bounds of ADR 0066 and the path fields
of the `GVF1` protocol; builds on the shared memory of ADR 0107.

## Context

Every layer bounded names and paths differently, and each bound was set by
whatever message the layer used: a component was at most 63 bytes
(`ramfs.MaxName`, the width of a directory reply), a VFS path 255 bytes, a
path in a `GVF1` request 192 bytes, a rename's target 32 bytes (it travelled
in the write-data field), the shell's working directory and the exec protocol
192 bytes, and a command line 128. A path that one layer accepted another
refused, and FAT32 volumes written elsewhere held names this system could not
open (GoOS 0.1.3 specification §8).

## Decision

**One contract.** `internal/limits` defines `NameMax = 1024` bytes per
component and `PathMax = 4096` bytes per absolute path. The shell, the
tokenizer, the exec protocol, the startup object, the VFS, RAMFS, the backend
protocol all use these constants and nothing smaller. FAT32 independently
applies its format limit of 255 UTF-16 code units.

**Names are bytes.** A name is any bytes except NUL and `/`, other than `.`
and `..`. There is no encoding at this layer. The FAT32 service additionally
requires printable ASCII outside `"*/:<>?\|`, one UTF-16 unit per byte, so its
255-unit VFAT limit is the same 255 bytes; on-disk long names holding other
characters are shown by their 8.3 alias. UTF-8 conversion for FAT32 is a later
change.

**Too long is a typed error.** A path over `PathMax` or a component over
`NameMax` is answered with `StatusNameTooLong`, not `InvalidArgument`, and the
utilities print "file name too long".

**Long paths travel in memory, not in bigger messages.** `GVF1` version 5
carries a path and a rename target, each in its own field, inline when they fit
the 256-byte payload. Otherwise the client writes them to a two-page object it
owns (`userspace/pathpage`), attaches a read-only copy of the handle, and marks
the request; the VFS maps the object, copies the paths into its own buffers
before checking them, and unmaps it. A listed name longer than a reply holds
(224 bytes) is answered with `StatusNameTooLong` and its length; the client
repeats the listing with a writable copy attached, and the VFS writes the name
at the object's start. The shell, PID 1, and every utility use the path page
through their VFS call; a process that never meets a long path never creates
it. With the rename target out of the data field, inline writes carry up to
224 bytes instead of 32.

**Long names reach the filesystem service through the file cache.** Version 5
of the backend protocol adds a name page after the cache's data slots. A
request or reply whose name does not fit its message (216 and 224 bytes) puts
the name there and says so; the receiver copies it out before validating it.
One exchange is in flight at a time, so one page serves both directions.

**The resolver stays allocation-free.** The VFS canonicalizes into its own
`PathMax` buffers, and `..` backs over the output instead of keeping a table of
component starts. The resolution cache keeps paths of up to 256 bytes; longer
ones are resolved each time rather than giving every cache entry a 4 KiB
buffer.

## Alternatives

- **A larger IPC payload.** Every queue slot of every channel would pay for the
  rare long path.
- **Splitting a path across messages.** It needs reassembly state per session
  and a protocol for abandoning it; one attached object needs neither.
- **UTF-8 names on FAT32 now.** It needs a case-folding policy for non-ASCII
  names and a UTF-16 to UTF-8 bound that can exceed 255 bytes; it is a change
  to the FAT32 service alone and can follow.

## Consequences

- A 1024-byte native name and a path of up to 4096 bytes work through RAMFS
  and the VFS. FAT32 accepts the subset representable in 255 UTF-16 units.
- RAMFS nodes trade memory for the larger fixed-width name field.
- A process that uses long paths pays for two pages of shared memory once.
- The utilities' error messages hold a whole path.
