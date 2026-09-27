# ADR 0121: File-backed executable sources

## Status

Accepted. Supersedes the whole-file preparation path in ADR 0097 and extends
ADR 0106 and ADR 0119.

## Context

Demand paging previously began only after PID 1 had copied the complete file
into kernel-owned executable pages. Large files therefore consumed their full
size in RAM before launch, even when the process touched only a few pages.

The kernel must not resolve filesystem paths or parse FAT32. VFS and FAT32 must
remain userspace services. Bytes used after image validation must also remain
stable across rename and concurrent writers.

## Decision

PID 1 opens the file through a private VFS session. VFS converts that open
handle to an executable open object: writes and truncation of the same object
are refused until the source closes. Rename remains valid, and the stable open
object follows it. FAT32 already refuses unlink while an object is open.

Preparation copies at most one 4 KiB header page into a sealed executable
object. PID 1 then calls `ExecutableSource`, which consumes the VFS session and
changes that object into a read-only source for the complete file. Its Execute
capability, stable VFS handle, size, and endpoint are the launch authority. The
kernel stores no pathname.

The native loader reads the header and segment table from the retained page,
validates bounds, overlap, user addresses, and W^X, then records source offsets
in image regions. It allocates no segment payload pages at launch.

On a missing image page, the kernel sends a bounded random-read request over
the source endpoint and blocks the faulting task. VFS reads through the stable
open object into the shared file-cache transport page. The reply is consumed
inside the send path before that page can be reused. The kernel copies the
requested bytes into a fresh process page, zeroes the remainder, applies the
segment's final permissions, and wakes the task. BSS pages remain demand-zero
and require no VFS request.

The source owns the VFS endpoint. The process holds a source reference while
any of its image regions may fault. Dropping the last reference closes the
endpoint; VFS session teardown releases the open object and its mutation pin.
A VFS failure makes the pending fault fail instead of substituting bytes from
another source.

## Consequences

A file-backed executable reserves one header frame and its page-list index
before launch, regardless of file size. Payload RAM is charged to the process
only for pages it faults in. A padded 12 MiB native image starts without reading
its untouched padding.

Execution is independent of the original pathname and survives rename. It
does not provide Unix write-while-executing semantics: writes, truncation, and
unlink are refused while a source exists.

Page faults now depend on the VFS and filesystem services. Their existing
deadlines and peer-death behavior turn service failure into a process fault;
the kernel does not wait without bound or fall back to pathname lookup.

System-image executables keep their immutable initramfs extent and shared
read-only segment cache. The new path applies to programs opened through VFS.
Linux syscall and Linux ELF handling remain separate work.
