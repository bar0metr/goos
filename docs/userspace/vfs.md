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

Paths are absolute and bounded to 255 bytes. Resolution collapses repeated
slashes and `.`, applies `..` without escaping the namespace root, and rejects
NUL and oversized components. VFS translates the remaining components into
opaque backend node operations. Kernel APIs and RAMFS never parse POSIX paths.

Open-file handles carry generations, backend/mount identity, independent
offsets, and explicit read/write rights. Closing a handle increments its
generation. Reads and writes advance only that open object's offset.

The fixed resolver cache stores resolved file and directory nodes. Mount and
metadata mutations invalidate it synchronously, preventing rename/unlink from
leaving stale path aliases. Cached and uncached resolution have separate
benchmarks. Path components cross the backend interface as fixed-size values,
so canonicalization remains on the caller stack and both resolver paths allocate
zero bytes per request.

Protocol `GVF1` version 2 carries bounded path, handle, offset, size, rights, and
inline data fields in typed IPC requests. Replies carry typed status, metadata,
directory entries, open handles, cursors, and at most 224 inline data bytes.
Version 2 keeps why a path was refused — not a directory, is a directory, not
empty, or a mount depends on it — instead of folding those into an invalid
argument (ADR 0079).
Bulk file data will use shared memory.

PID 1 supervises `/system/vfs` and starts the shell only after the service is
healthy. The service owns its RAMFS backend and namespace in its own address
space. Its initial namespace contains `/tmp` in RAMFS, the read-only boot archive at
`/system`, and the persistent volume at `/persist`, `/home` and `/state`. Kernel launch
grants explicitly identify the client or server side of a channel; neither
process infers endpoint identity from handle numbers or launch order.
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
