# Filesystem stack

Files reach applications through four isolated components. None of them runs
in the kernel, and none holds more authority than its layer needs.

```text
utility ──GVF1──▶ VFS service ─┬─ RAMFS backend            (/, /tmp)
                               ├─ image backend            (/system, read-only boot archive)
                               └─ FAT32 backend ──GBLK001──▶ block service ──IPC──▶ virtio-blk driver ──▶ disk
                                  (/persist, /home, /state)
```

| Component | Process | Holds | Details |
| --- | --- | --- | --- |
| VFS | `/system/vfs` | control endpoint, one server endpoint per session, block client endpoint | [vfs.md](../userspace/vfs.md), ADR 0018, 0026 |
| RAMFS | inside VFS | nothing; library core | [ramfs.md](../userspace/ramfs.md), ADR 0017 |
| System image | inside VFS | image capability, read-only mapping of the boot archive | ADR 0070 |
| FAT32 | inside VFS | uses the VFS block client endpoint | [storage.md](../userspace/storage.md), ADR 0036 |
| Block service | `/system/block` | public and filesystem server endpoints, driver client endpoint | [storage.md](../userspace/storage.md), ADR 0033 |
| virtio-blk | `/system/virtio-blk` | port range, IRQ 11, DMA domain, driver server endpoint | [storage.md](../userspace/storage.md), ADR 0034, 0035 |

## Namespace

VFS owns the namespace and all path handling. Paths are absolute, at most 255
bytes, and canonicalized by VFS (repeated slashes, `.`, `..` clamped at the
root). Backends see only opaque node IDs and single path components; neither
the kernel nor RAMFS nor FAT32 parses POSIX paths. Mount selection uses the
longest whole-component prefix.

| Path | Backend | Persistence |
| --- | --- | --- |
| `/` | RAMFS | volatile |
| `/tmp` | RAMFS | volatile |
| `/system` | boot archive, read-only | immutable; replaced only by a new image |
| `/persist` | FAT32 over the virtio-blk disk | survives reboot; the whole volume |
| `/home` | the volume's `home` directory | survives reboot; user data |
| `/state` | the volume's `state` directory | survives reboot; mutable system state |

A mount appears as a directory in the listing of its parent, so the layout is
found by walking the namespace rather than by knowing a path in advance. A
directory another mount points at cannot be removed, by path or under an alias
such as `/persist/home` (ADR 0069).

`/system` is the boot archive itself, mounted read-only, so the executables the
launch path resolves under `/system/bin` are exactly the ones a listing shows.
Every mutation of it is refused with permission denied. Executables are still
not *loaded* through VFS: the kernel reads them from the same archive on PID 1's
launch request, and the filesystem service maps that archive read-only through a
capability only it holds (ADR 0070).

`/persist` names the whole volume and `/home` and `/state` name directories on
it, so `/persist/home` and `/home` are one directory under two names, backed by
one filesystem instance and one sector cache (ADR 0071).

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

- A volume whose declared geometry does not fit its device, or whose FAT cannot
  describe the clusters it claims, is refused at mount. The system still boots:
  `/persist`, `/home` and `/state` are absent and everything else works
  (ADR 0077).
- Each hop validates sizes, offsets, and reserved fields before acting.
- Services reply `InvalidArgument` to requests they can attribute but not
  parse.
- Every storage wait is bounded; an expired wait becomes an I/O error.
- A crashed service closes its endpoints on teardown, and peers observe
  `PeerDied`. A dead peer is dropped rather than treated as fatal: a service
  outlives the processes that use it.
- The driver restarts (`restart: on-failure`). PID 1 creates a fresh channel,
  tells the block service over a control endpoint, and launches the replacement
  with the same device authority, which it kept rather than consumed. The block
  service and the filesystem service do not restart: their clients hold sessions
  that would each have to be rebuilt (ADR 0078).

## Names

A path component is at most 63 bytes (`ramfs.MaxName`), the width of a VFS
directory reply. FAT32 stores a component that fits the 8.3 form — lower case,
at most eight characters and a three-character extension — in a single
directory entry, exactly as before. Any other component gets a generated
`NAME~1` alias plus a VFAT long-name chain in front of it, so case, length, and
the wider character set survive a round trip (ADR 0066). A chain whose
ordinals, padding, or checksum do not match is ignored and the alias is used
instead. Names longer than the bound, or holding characters the system cannot
carry, are rejected with `InvalidArgument`.

## Limits

FAT32 supports only 512-byte sectors, one-sector clusters, one FAT, four
clusters per directory (64 entries, of which a long name costs up to six), and
files up to 64 KiB. Crash consistency, shared-memory bulk transfers, and a
separate filesystem process are future work.
