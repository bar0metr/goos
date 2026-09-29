# Filesystem stack

Files reach applications through five isolated components. None of them runs
in the kernel, and none holds more authority than its layer needs.

```text
utility ──GVF1──▶ VFS service ─┬─ RAMFS backend            (/, /tmp)
                               ├─ image backend            (/system, read-only boot archive)
                               └─ remote backend ──GFSPRT01──▶ FAT32 service ──GBLK001──▶ block service ──IPC──▶ virtio-blk driver ──▶ disk
                                  (/persist, /home, /state)                      (one slice)   (partition table)
```

| Component | Process | Holds | Details |
| --- | --- | --- | --- |
| VFS | `/system/vfs` | control endpoint, one server endpoint per session, client end of the FAT32 channel, file cache mapping | [vfs.md](../userspace/vfs.md), ADR 0018, 0026, 0096, 0098 |
| RAMFS | inside VFS | a page pool in the VFS's address space | [ramfs.md](../userspace/ramfs.md), ADR 0017, 0105 |
| System image | inside VFS | image capability, read-only mapping of the boot archive | ADR 0070 |
| FAT32 | `/system/fat32` | server end of the VFS channel, a block client endpoint bounded to its partition, file cache mapping | [storage.md](../userspace/storage.md), ADR 0036, 0096, 0098, 0115 |
| Block service | `/system/block` | public and filesystem server endpoints, driver client endpoint; reads the partition table and serves the filesystem endpoint through a slice | [storage.md](../userspace/storage.md), ADR 0033, 0115 |
| virtio-blk | `/system/virtio-blk` | port range, IRQ 11, DMA domain, driver server endpoint | [storage.md](../userspace/storage.md), ADR 0034, 0035 |

## Namespace

VFS owns the namespace and all path handling. Paths are absolute, at most 4096
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
ADR 0093). For a program elsewhere, PID 1 opens a private VFS session and has
VFS pin the stable open object against mutation. The kernel retains one header
page and requests later pages by offset as the process faults them in. The
source capability contains no path (ADR 0106, ADR 0119, ADR 0121).

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

## Native metadata

VFS `stat` and directory replies use one native fixed-width metadata model
(ADR 0126): object kind, 64-bit size, optional UTC creation and modification
timestamps, stable attributes, and a namespace mount ID. Availability bits
distinguish absent timestamps or attributes from real zero values. The
attributes are directory, read-only, hidden, system and archive; no FAT raw
byte or invented Unix ownership/mode field crosses the boundary.

FAT32 translates its directory-entry fields in the filesystem service. RAMFS
uses the native realtime clock for creation and modification events. The
system image reports its read-only state and explicitly leaves timestamps
unavailable. Mount IDs are descriptions assigned by VFS, not handles and not
authority.

## Capacity and mounts

`FilesystemInfo` reports a stable filesystem type, 64-bit total, free and
available byte counts, read-only state, explicit capacity availability and an
optional bounded volume label (ADR 0127). A client selects the mounted
filesystem by canonical path or generation-safe mount ID. The fixed VFS mount
table is enumerated with a bounded cursor; entries expose only descriptive IDs,
paths and filesystem properties, never backend handles or endpoints.

RAMFS derives capacity from its configured page quota and current page use.
The immutable system image reports its archive length and zero reusable bytes.
FAT32 derives total bytes from validated cluster geometry with checked 64-bit
arithmetic and refreshes an unknown free count from the FAT rather than trusting
the advisory FSInfo sector.

The `df` and `mount` commands receive only an ordinary VFS session. `df` uses
mount enumeration followed by an information query for current capacity;
`mount` prints the bounded descriptive entries and has no mutation mode.
Formatting uses integer binary units, and explicitly prints `-` when capacity
is unavailable.

## Request path

A `cat /persist/a.txt` stage issues `GVF1` open, read, and close requests on its
session. VFS resolves the path to the FAT32 mount and sends each backend call
as a `GFSPRT01` request to the FAT32 service, which translates the file range
into cluster runs. Each run becomes a `GBLK001` request on the FAT32 service's
block client endpoint, and the block service forwards it to the driver over a
private channel. The driver performs the virtqueue transfer and waits for
IRQ 11. Replies unwind in reverse.

Small requests carry their data inline. Metadata and capacity replies use the
GVF1 version 10 fixed header; long names and mount paths continue through an
attached object. Write
requests retain their existing bounded transport. Large transfers move through
shared memory instead
(ADR 0107):

- A client creates a buffer with `MemoryCreate`, maps it, and attaches to each
  `GVF1` `ReadInto` or `WriteFrom` a copy of its handle narrowed with
  `HandleDuplicate` to the rights the VFS needs. The VFS maps the buffer for
  that request only. `cp` and `bench io` move 64 KiB per exchange this way.
- Executable preparation fills only a one-page sealed header object. Later
  page faults use bounded offset reads through the source's private VFS
  session and the shared file-cache transport page (ADR 0121).
- File data between the VFS and the FAT32 service moves through the file
  cache, a shared-memory object both map: 128 KiB of data slots and a page
  for names too long for a message (ADR 0098, ADR 0110). A `GFSPRT01` read
  or write names a run of up to 16 consecutive slots; the VFS serves later
  reads of a cached page without asking the service, and writes go through to
  the service before they return.
- The FAT32 service moves runs of up to 128 KiB to and from the block service
  with `ReadBulk` and `WriteBulk`, through a buffer of its own; the block
  service copies through a buffer it shares with the driver, whose DMA region
  has a 128 KiB data area. Requests of up to 232 bytes stay inline.

FAT32 keeps a sixteen-sector window of metadata to avoid repeated FAT and
directory reads, and gathers FAT changes there to write a sector at a time
(ADR 0048, ADR 0102, ADR 0107).

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

A path component is at most 1024 bytes and a path at most 4096, in every layer
(`limits.NameMax`, `limits.PathMax`; ADR 0110). Names are bytes other than NUL
and `/`. A name too long for a message crosses to the VFS in a memory object
the client attaches, and between the VFS and the FAT32 service in the name
page of the file cache. FAT32 names are UTF-8 natively and UTF-16LE on disk;
the filesystem independently enforces VFAT's 255 UTF-16-unit limit. FAT32 stores a component that fits the 8.3 form, each
part in one case, in a single directory entry with the Windows NT case flags.
Any other component gets a generated `NAME~1` alias plus a VFAT long-name chain
in front of it, so case, length and any Unicode character survive a round trip
(ADR 0066, ADR 0112). Names are not normalized and are matched
case-insensitively through Unicode simple one-to-one uppercase mapping. A chain whose ordinals, padding, checksum or UTF-16 do not
match is ignored and the alias is used instead. Native components longer than
1024 bytes are rejected with `NameTooLong`; FAT names over 255 UTF-16 units,
invalid UTF-8, or reserved characters are `InvalidArgument`. GoOS refuses to
create trailing-space or trailing-dot FAT names, but valid foreign names of
that form remain listable and exactly openable, without Win32 trimming.

## Limits

FAT32 reads volumes with 512, 1024, 2048 or 4096-byte logical sectors, 512-byte
to 512 KiB clusters, and one or two FATs, mirrored or with one active copy.
Files reach the FAT32 maximum of 4 GiB − 1, directories the FAT maximum of 65536
entries, and every chain walk is bounded by the volume's cluster count, so
corrupt media ends in a typed error (ADR 0102, ADR 0112). A corrupt primary boot
sector with a valid backup is mounted read-only from the backup. There is no
journal: changes are ordered so that an interruption leaves only states a
filesystem checker repairs.
