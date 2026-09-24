# ADR 0093: Executable-image launch authority

## Status

Accepted. Amends ADR 0022, ADR 0030, ADR 0070 and ADR 0081.

## Context

Every launch after PID 1 named its executable by path. PID 1 sent
`/system/bin/ls` on the lifecycle channel, and the kernel stripped the slash,
walked the USTAR boot archive with `initramfs.Locate`, followed hard links, and
loaded what it found. The kernel therefore owned three pieces of userspace
policy: the executable namespace (`/system/...`), the archive format as a
lookup structure, and which paths may be launched at all.

That was harmless while the boot archive was the only place executables could
come from, but it is the wrong direction for what comes next. Executables on
persistent storage, and Linux ELF programs after them, would each have taught
the kernel another path syntax or filesystem. The specification keeps path
resolution and filesystems out of the kernel, and the userspace VFS already
serves the same archive at `/system` (ADR 0070).

The authority to launch was also implicit. Holding the lifecycle channel was
enough to run any member of the archive; nothing named the right to execute
from it.

## Decision

**The kernel is given bytes, not a path.** A launch request (lifecycle protocol
version 7) carries an `Executable`: a handle to an image object, a byte offset,
and a length. It also carries a display name of at most sixteen printable bytes
with no `/`, which the kernel records for `ps` and resolves nothing from.

**Execute on the image object is the authority to launch from it.** The kernel
looks the handle up in PID 1's table as a device handle with `Execute` and
requires the object to be the system image. It then checks that the extent lies
inside the image, without overflow, and hands the bytes to the native image
validator, which decides whether they are a program. An extent that covers a
data file, half a program, or two members fails there.

**PID 1 is the executable source.** It maps the system image read-only with
`ImageMap` before it launches anything, because the filesystem service is
itself launched from the image and cannot be asked for its own executable.
`exec.LocateImage` applies the search policy: the path must be canonical and
under `/system/`, and it is resolved in the archive, hard links included.
A path the image does not hold is answered with "command not found" by PID 1
and never reaches the kernel; a path outside `/system/` is refused as
permission denied, since the image is the only executable source so far.

**PID 1 keeps the image, and the filesystem service reads it.** PID 1 holds the
image capability with `Read|Execute|Transfer`. It grants the filesystem service
a `Read` copy instead of consuming its own, so the service that serves
`/system` cannot launch from it and does not need to.

**Bootstrap stays special, and only bootstrap.** The kernel still finds
`system/init` in the archive with `initramfs.Find`, because there is nobody
else to find it. That is the one name the kernel knows.

**Shared image pages are keyed by extent.** The shared-page cache (ADR 0081)
used the member's archive offset as its key. It now uses the extent,
`offset<<32 | length`. The image is immutable, so an extent always names the
same bytes; two extents that start together but differ in length get
different keys, so a short extent can never hit frames a longer one filled.

## Alternatives

**Asking VFS for every executable.** PID 1 would open the path through VFS and
pass the kernel an object VFS produced. It is the long-term shape for
executables on persistent storage, but it puts VFS on the critical path of its
own launch and of every command, and the archive is already mapped memory. The
extent interface does not preclude it: a future source can hand the kernel a
memory object instead of an extent of the image.

**Copying each executable into a shared-memory object.** PID 1 would read the
bytes and pass a memory handle. That doubles every image in memory, is bounded
by `shmem.MaxPages`, and loses the shared read-only pages that make launches
fast (ADR 0081).

**Keeping the path and dropping only the `/system/bin` prefix check.** The
kernel would still parse the archive as a namespace, which is the thing to
remove.

**A per-executable capability.** The kernel could mint one handle per member.
It would need a registry of members, i.e. the archive parser in the kernel
again, for no authority the image-wide right does not already express: every
member of the system image is equally trusted.

## Consequences

The normal launch path contains no path handling and no USTAR lookup; the only
`initramfs` call left in the kernel is the bootstrap `Find`, which also serves
the validation self-tests. `initramfs.Locate` is gone; `initramfs.Extent`
reports where a member's bytes lie.

The validation build proves the authority rules from PID 1
(`LAUNCH_AUTHORITY_OK`): a launch naming the console handle, a handle PID 1
does not hold, an extent past the end of the image, a wrapping extent, the
bytes of `/system/motd`, and half an executable are all refused with no process
created, and paths outside `/system` or missing from it are answered by PID 1.

The `launch failed` record for a missing command now comes from PID 1 and names
the command (`init[1]: launch failed command=missing-stage status=2`); the
kernel still records failures of the launches it performs.

PID 1 maps the image, which costs it two leaf page tables of its page quota and
no memory for the image itself. A missing command no longer costs a kernel
round trip. Launch latency is otherwise unchanged: the kernel's former archive
scan becomes PID 1's, over the same resident bytes.

Executables on persistent storage remain unsupported. When they arrive they
need a second executable source in userspace and a kernel launch from a memory
object, not a filesystem in the kernel.
