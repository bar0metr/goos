# ADR 0026: Capability-bound shell VFS session

## Status

Accepted

## Decision

Run the first live VFS boundary as a supervised native userspace service. PID 1
launches `/system/vfs` before `/system/shell` and gives them opposite sides of
one channel through explicit endpoint metadata in the launch capability
manifest. Child capabilities omit `Transfer` authority.

Complete the `GVF1` request/reply contract with bounded typed replies. The shell
owns its working-directory state, canonicalizes relative paths in a fixed
buffer, and implements `pwd`, `ls`, `cd`, and `cat` through correlated VFS IPC.
The service owns a private RAMFS-backed namespace containing `/system/motd` and
`/tmp`.

## Consequences

Filesystem policy and namespace state execute outside the kernel. Endpoint
identity is explicit and cannot depend on handle layout or service launch order.
The shell has only the console and VFS authorities it needs and cannot delegate
either.

The initial VFS namespace is volatile and is not the executable initramfs.
Persistent storage, an initramfs filesystem backend, shared-memory bulk I/O,
permissions, and process launch remain separate work.

Adding the VFS image raises the bounded UEFI initramfs reservation from 512 KiB
to 1 MiB. The loader still performs one bounded read and rejects empty or
oversized archives.
