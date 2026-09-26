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
generation. Reads and writes advance only that open object's offset.

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

PID 1 supervises `/system/vfs` and starts the shell only after the service is
healthy. The service owns its RAMFS backend and namespace in its own address
space. Its initial namespace contains `/tmp` in RAMFS, the read-only boot archive at
`/system`, and the persistent volume at `/persist`, `/home` and `/state`. The
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
restarted one is attached under the same mounts and open files (ADR 0096). Kernel launch
grants explicitly identify the client or server side of a channel; neither
process infers endpoint identity from handle numbers or launch order.

Clean unmount is a distinct PID 1-only control operation (ADR 0116). It flushes
the filesystem first, refuses while any open VFS handle refers to any alias of
the backend, then removes `/persist`, `/home` and `/state` atomically and closes
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
