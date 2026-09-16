# Userspace VFS

Each VFS namespace owns a bounded mount table, resolver cache, and open-file
table. Mount selection uses the longest complete-component prefix. `/dev` can
therefore override a subtree without capturing `/devx`, and separate namespaces
can expose different mounts over the same underlying filesystem.

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

Protocol `GVF1` version 1 carries bounded path, handle, offset, size, rights, and
inline data fields in typed IPC requests. Replies carry typed status, metadata,
directory entries, open handles, cursors, and at most 224 inline data bytes.
Bulk file data will use shared memory.

PID 1 supervises `/system/vfs` and starts the shell only after the service is
healthy. The service owns its RAMFS backend and namespace in its own address
space. Its initial namespace contains `/system/motd` and `/tmp`. Kernel launch
grants explicitly identify the client or server side of a channel; neither
process infers endpoint identity from handle numbers or launch order.
