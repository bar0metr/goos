# Userspace VFS

Each VFS namespace owns a bounded mount table, resolver cache, and open-file
table. Mount selection uses the longest complete-component prefix. `/dev` can
therefore override a subtree without capturing `/devx`, and separate namespaces
can expose different mounts over the same underlying filesystem.

A directory listing runs in two phases. Cursors below `1 << 30` belong to the
backend that owns the directory; once it is exhausted the scan continues through
the mount table and reports the mounts whose path names a direct child of that
directory. A mount therefore appears as a directory in its parent's listing
without any placeholder existing in the parent filesystem, and a name the
backend already listed is not reported twice. Removing a path that a mount sits
at or below is refused, and so is removing a node some mount uses as its root,
which is what protects a mount reachable under a second name (ADR 0069).

Paths are absolute and bounded to 4096 bytes, components to 255 (ADR 0110).
Resolution collapses repeated slashes and `.`, applies `..` without escaping
the namespace root, and rejects NUL; a path or component over the limits is
answered with `StatusNameTooLong`. VFS translates the remaining components into
opaque backend node operations. Kernel APIs and RAMFS never parse POSIX paths.

Open-file handles carry generations, backend/mount identity, independent
offsets, and explicit read/write rights. Closing a handle increments its
generation. Reads and writes advance only that open object's offset. A backend
that provides stable open objects replaces the pathname lookup node with an
opaque live-object cookie for the handle lifetime (ADR 0119). FAT32 moves that
object with its directory entry, so an open handle survives rename across
directories. Unlink of an open FAT32 file is refused until its last handle is
closed; delete-while-open is not part of the native 0.1.4 contract.

Every open file belongs to the session that opened it. A handle is valid only
on that session: another session's handle is answered as not found, so one
client can neither use nor probe the files another holds. A session holds at
most 64 of the 256 open files (ADR 0111), and when its channel closes, because the client
exited or crashed, every file it left open is closed with it before its slot
is reused. Until 0.1.2 the table was shared by all sessions and a file left
open by a dead process stayed open for the rest of the boot.

The fixed resolver cache stores resolved file and directory nodes whose paths
are at most 256 bytes; longer paths are resolved each time. Mount and
metadata mutations invalidate it synchronously, preventing rename/unlink from
leaving stale path aliases. Cached and uncached resolution have separate
benchmarks. Path components cross the backend interface as fixed-size values,
so canonicalization remains on the caller stack and both resolver paths allocate
zero bytes per request.

Protocol `GVF1` version 5 carries path, rename target, handle, offset, size,
rights, and inline data fields in typed IPC requests. Replies carry typed
status, metadata, directory entries, open handles, cursors, and at most 224
inline data bytes. Version 2 keeps why a path was refused — not a directory,
is a directory, not empty, or a mount depends on it — instead of folding those
into an invalid argument (ADR 0079). Version 4 moves bulk file data through
an attached memory object (ADR 0107). Version 5 lets paths too long for a
message travel in an attached object the client owns, and returns a listed
name longer than 224 bytes in one when the client attaches it (ADR 0110).
Versions 9 and 10 add file metadata, filesystem capacity and mount enumeration
(ADR 0126, 0127).

Version 11 adds `Replace`, which moves a regular file onto an existing one of
the same filesystem and removes the old one (ADR 0129). Rename still refuses an
existing target. The VFS refuses to replace a file any session holds open, a
directory or a mount root; a missing target makes `Replace` a rename. Editors
save through it: they write the new contents to `.<name>.~save` beside the file
and replace only when that file is complete, so a failed save leaves the
original as it was. On FAT32 the replacement ends in a device flush and never
leaves the target holding part of either version.

Version 12 lets a `Stat` with a handle and no path describe that open file
(ADR 0141). The answer follows the file through a rename, and a client learns
the length of its open file without keeping the path. The Linux compatibility
layer uses it for `lseek` from the end and for appending writes.

Version 13 returns a node's identity with a `Stat` and with a directory entry,
in the reply field an `Open` uses for its handle (ADR 0142). Two answers about
one file of a mount carry the same value. An open file keeps the identity of
its path and takes the new one when it is renamed, so a stat by handle and a
stat by path agree on filesystems that name open files by a cookie.

The validation build answers one name late: a request for
`/tmp/.validation-vfs-stall` gets its reply only when the same session sends
its next request. The boot test uses it to watch a client's deadline expire.
The normal build has no such name.

PID 1 supervises `/bootstrap/vfs` and starts the shell only after the service is
healthy. The service owns its RAMFS backend and namespace in its own address
space. Its initial namespace contains `/tmp` and `/run` in RAMFS, the
read-only boot archive at `/bootstrap`, and the persistent volume at
`/persist`, `/home` and `/state`; PID 1 later asks it to publish the installed
system at `/system` and `/apps` (ADR 0175). The
persistent volume is served by the FAT32 service, a separate process: VFS
reaches it through a remote backend over the `GFSPRT01` protocol, and mounts it
when PID 1 hands over the channel with the `SetFilesystem` control operation.
File data moves through the file cache the two map (ADR 0098): the VFS indexes
which slot holds which page and serves reads of cached pages itself.

The service restarts on failure, and exits rather than spinning when it cannot
go on (ADR 0099). PID 1 launches the new instance with a new control endpoint
and first session and hands the shell, config and log services new sessions
with a session notice over channels they already hold; the FAT32 service
restarts behind it and is mounted again. `/tmp` starts empty, and files open
in the old instance are gone.
A dead or stalled FAT32 service is an I/O error on those three mounts only; a
restarted one is attached under the same mounts. After an unanswered accepted
request, the VFS quarantines the cache and asks PID 1 to replace the service.
PID 1 kills that instance and waits for the kernel to report its exit before
sending a fenced attach (ADR 0204). In validation builds, the FAT32 service
stalls on a request for `.validation-fat32-stall`; the boot test uses that
name to check recovery. Stable FAT32 object cookies
belong to the service instance that issued them, so callers must reopen old
handles after a FAT32 service restart (ADR 0119). Kernel launch
grants explicitly identify the client or server side of a channel; neither
process infers endpoint identity from handle numbers or launch order.

When it mounts a volume the service looks up the volume's `system` directory
and asks the namespace to protect it (ADR 0175). The namespace records the
directory as a node of the backend, and every walk notes whether it passed
through that node, so the protection holds under every name that reaches it:
`/system`, `/persist/system`, case variants FAT32 resolves to the same entry,
and the mount of the directory itself. Create, writable open, truncate,
unlink, rename, and replace at or below it are refused with
`StatusPermissionDenied`, a rename or replace into or out of it too, and stat
reports its files read-only. The VFS keeps `/bootstrap`, `/system`, and
`/apps` reserved: nothing can be created at or below them, or moved there,
while nothing is mounted there.

Control protocol version 7 adds two PID 1-only operations. `Publish` mounts
the protected system directory at `/system` and the volume's `apps`
directory, made if missing, at `/apps`; both appear or neither does, and a
volume without a protected system directory is refused. `Withdraw` removes
both, and is busy while a file is open through either.

Clean unmount is a distinct PID 1-only control operation (ADR 0116). It flushes
the filesystem first, refuses while any open VFS handle refers to any alias of
the backend, then removes every mount of it, `/system` and `/apps` included, atomically and closes
the service channel. A dead filesystem service is not treated as a clean
unmount: its mounts remain and return peer errors so supervision can reattach a
replacement without invalidating namespace identity.
The freestanding runtime uses a fixed bump allocator. After persistent service
initialization, VFS records an arena mark and restores it before every request;
temporary decode and dispatch allocations therefore remain request-bounded.

VFS also owns a control endpoint. PID 1 creates a new channel pair for every
external process and moves the server endpoint to VFS in an attached IPC handle.
The client endpoint is consumed by the child's launch manifest. VFS maintains a
fixed table of active sessions. One bounded `WaitAny` operation covers the
control endpoint and every active session, and its ready mask identifies the
endpoints to receive from. VFS closes a server endpoint on `PeerDied`, and the
kernel then releases the channel object. Replies therefore cannot be consumed
by another process sharing one queue.
