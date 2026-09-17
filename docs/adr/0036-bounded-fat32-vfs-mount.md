# ADR 0036: Bounded FAT32 VFS mount

## Status

Accepted.

## Context

The persistent virtio-blk data path ended at a byte-oriented block service.
Applications still saw only the volatile RAMFS namespace, and giving VFS direct
device authority would collapse the filesystem, block-service, and driver
boundaries.

## Decision

VFS receives a dedicated client endpoint to the block service and mounts a
bounded FAT32 backend at `/persist`. The block service serves this endpoint and
the PID 1 validation endpoint independently; both remain behind the same
hardware-independent `GBLK001` protocol.

The first FAT32 implementation deliberately supports only the root directory,
8.3 ASCII names, 512-byte sectors, one FAT, one-sector clusters, sixteen root
entries, and files up to 64 KiB. It validates the boot parameters before use,
walks and updates FAT chains with bounded loops, and implements create, open,
read, write, truncate, rename, unlink, stat, and directory enumeration. The
host creates a deterministic 64 MiB raw FAT32 image when none exists; normal
builds preserve the existing image.

The VFS and block-service adapters use separate IPC wire buffers for nested
requests. Pointer-based IPC codecs reuse preallocated messages in the block and
driver hot paths, while the block and VFS services reset their bounded TinyGo
allocation arenas between requests. The block service alternates blocking waits
across its two endpoints and processes the message that wakes it, preventing
request loss and starvation.

## Consequences

Native utilities can persist regular files under `/persist` without receiving
block or device capabilities. QEMU validates the complete VFS-to-FAT32-to-block-
service-to-virtio path, verifies the resulting bytes in the raw image, starts a
fresh VM, and reads the file without rewriting it. Host tests also remount the
same device and read data written before the remount.

Subdirectories, long-file-name creation, multiple FAT copies, variable cluster
sizes, crash consistency, repair, concurrent requests, and a separate
filesystem process remain future work. The bounded FAT32 backend currently
runs inside the VFS process, but the device boundary remains capability-scoped.
