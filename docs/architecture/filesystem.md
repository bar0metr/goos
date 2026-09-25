# Filesystem stack

Files reach applications through five isolated components. None of them runs
in the kernel, and none holds more authority than its layer needs.

```text
utility ──GVF1──▶ VFS service ─┬─ RAMFS backend            (/, /tmp)
                               ├─ image backend            (/system, read-only boot archive)
                               └─ remote backend ──GFSPRT01──▶ FAT32 service ──GBLK001──▶ block service ──IPC──▶ virtio-blk driver ──▶ disk
                                  (/persist, /home, /state)
```

| Component | Process | Holds | Details |
| --- | --- | --- | --- |
| VFS | `/system/vfs` | control endpoint, one server endpoint per session, client end of the FAT32 channel, file cache mapping | [vfs.md](../userspace/vfs.md), ADR 0018, 0026, 0096, 0098 |
| RAMFS | inside VFS | a page pool in the VFS's address space | [ramfs.md](../userspace/ramfs.md), ADR 0017, 0105 |
| System image | inside VFS | image capability, read-only mapping of the boot archive | ADR 0070 |
| FAT32 | `/system/fat32` | server end of the VFS channel, block client endpoint, file cache mapping | [storage.md](../userspace/storage.md), ADR 0036, 0096, 0098 |
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
not *loaded* through VFS: PID 1 maps the same archive read-only, resolves the
path there, and asks the kernel to launch the extent it found under an image
capability with Execute. The filesystem service maps the archive through a
Read-only copy of that capability, which cannot launch anything (ADR 0070,
ADR 0093). A program anywhere else in the namespace is read through the VFS,
by PID 1, into an executable object the kernel launches whole once it has
sealed (ADR 0097).

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
session. VFS resolves the path to the FAT32 mount and sends each backend call
as a `GFSPRT01` request to the FAT32 service, which translates the file range
into sector reads. Each read becomes a `GBLK001` request on the FAT32
service's block client endpoint, and the block service forwards it to the driver over a
private channel. The driver performs the virtqueue transfer and waits for
IRQ 11. Replies unwind in reverse.

File data between the VFS and the FAT32 service moves through the file cache,
a 128 KiB shared-memory object both map (ADR 0098): a `GFSPRT01` read asks the
service to fill a 4 KiB slot with a page of the file, a write names the slot
its bytes are in, and the VFS serves later reads of a cached page without
asking the service. Writes go through to the service before they return.
Elsewhere payloads are inline IPC data: at most 224 bytes per VFS reply and 232
bytes per block request. FAT32 keeps a sixteen-sector write-through window of
metadata to avoid repeated FAT and directory reads (ADR 0048, ADR 0102).

## Failure behaviour

- A volume whose declared geometry does not fit its device, or that is not a
  FAT32 volume this service can read, is refused at mount (ADR 0102). The system still boots:
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
  with the same device authority, which it kept rather than consumed (ADR 0078).
- Every other service in the stack restarts too. The FAT32 service is attached
  to the VFS again (ADR 0096); a restarted VFS hands its clients new sessions
  and has the FAT32 service restart behind it (ADR 0099); a restarted block
  service has the driver and the FAT32 service restart behind it (ADR 0101).
  What lived only in a dead process is gone: `/tmp` with the VFS, and a write
  in flight with whichever service died under it.

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

FAT32 reads volumes with 512-byte sectors, 512-byte to 64 KiB clusters, and one
or two FATs. Files reach the FAT32 maximum of 4 GiB − 1, directories the FAT
maximum of 65536 entries, and every chain walk is bounded by the volume's
cluster count, so corrupt media ends in a typed error (ADR 0102). There is no
journal: changes are ordered so that an interruption leaves only states a
filesystem checker repairs.
