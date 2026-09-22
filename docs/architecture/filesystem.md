# Filesystem stack

Files reach applications through four isolated components. None of them runs
in the kernel, and none holds more authority than its layer needs.

```text
utility ──GVF1──▶ VFS service ─┬─ RAMFS backend           (/, /system, /tmp)
                               └─ FAT32 backend ──GBLK001──▶ block service ──IPC──▶ virtio-blk driver ──▶ disk
                                  (/persist)
```

| Component | Process | Holds | Details |
| --- | --- | --- | --- |
| VFS | `/system/vfs` | control endpoint, one server endpoint per session, block client endpoint | [vfs.md](vfs.md), ADR 0018, 0026 |
| RAMFS | inside VFS | nothing; library core | [ramfs.md](ramfs.md), ADR 0017 |
| FAT32 | inside VFS | uses the VFS block client endpoint | [storage.md](storage.md), ADR 0036 |
| Block service | `/system/block` | public and filesystem server endpoints, driver client endpoint | [storage.md](storage.md), ADR 0033 |
| virtio-blk | `/system/virtio-blk` | port range, IRQ 11, DMA domain, driver server endpoint | [storage.md](storage.md), ADR 0034, 0035 |

## Namespace

VFS owns the namespace and all path handling. Paths are absolute, at most 255
bytes, and canonicalized by VFS (repeated slashes, `.`, `..` clamped at the
root). Backends see only opaque node IDs and single path components; neither
the kernel nor RAMFS nor FAT32 parses POSIX paths. Mount selection uses the
longest whole-component prefix.

| Path | Backend | Persistence |
| --- | --- | --- |
| `/` | RAMFS | volatile |
| `/system` | RAMFS | volatile, populated at VFS start |
| `/tmp` | RAMFS | volatile |
| `/persist` | FAT32 over the virtio-blk disk | survives reboot |

Executables are not loaded through VFS. The kernel reads them from the
immutable initramfs on PID 1's launch request.

## Sessions

Every process that uses files gets its own VFS session: a fresh channel whose
server end PID 1 hands to VFS over the control channel and whose client end is
consumed into the child's launch manifest. Replies therefore cannot be read by
another process. VFS serves the control endpoint and all active sessions from
one `WaitAny` loop and closes a session when its peer dies.

Open-file handles inside VFS are generation-safe, carry their own rights and
offset, and are per session.

## Request path

A `cat /persist/a.txt` stage issues `GVF1` open, read, and close requests on its
session. VFS resolves the path to the FAT32 mount, and FAT32 translates the
file range into sector reads. Each read becomes a `GBLK001` request on the
block client endpoint, and the block service forwards it to the driver over a
private channel. The driver performs the virtqueue transfer and waits for
IRQ 11. Replies unwind in reverse.

Payloads are inline IPC data: at most 224 bytes per VFS reply and 232 bytes
per block request. FAT32 keeps a four-sector write-through window to avoid
repeated metadata reads (ADR 0048).

## Failure behaviour

- Each hop validates sizes, offsets, and reserved fields before acting.
- Services reply `InvalidArgument` to requests they can attribute but not
  parse.
- Every storage wait is bounded; an expired wait becomes an I/O error.
- A crashed service closes its endpoints on teardown, and peers observe
  `PeerDied`. The storage services are not restarted today.

## Limits

FAT32 supports only a flat root directory, 8.3 names, 512-byte sectors,
one-sector clusters, one FAT, sixteen root entries, and files up to 64 KiB.
Subdirectories, long names, crash consistency, shared-memory bulk transfers,
and a separate filesystem process are future work.
