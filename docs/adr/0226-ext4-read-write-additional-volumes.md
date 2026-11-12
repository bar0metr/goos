# ADR 0226: ext4 read-write additional volumes

## Status

Accepted. Extends ADR 0222 (volume instances), ADR 0223 (links and Unix
metadata) and ADR 0225 (JBD2 journal and writable mounts). ADR 0227
replaces its durability rule: operations commit in batches. It replaces the
rule of ADR 0222 that additional volumes get read-only slices and that PID 1
never holds a writable slice for one, and the part of ADR 0225 that kept
the ext4 service read-only.

## Context

ADR 0225 gave `userspace/ext4` a journaled writer that survives power cuts
and that Linux can recover. No GoOS client could reach it: the block
service granted additional volumes read-only slices, the ext4 service
mounted read-only, the adapter refused every mutation, and the filesystem
protocol named a rename only by the node that moves. An ext4 inode records
no name, and with hard links one inode has several, so the service could
not tell which entry a rename meant.

The 0.3.0 release also has to say whether ext4 becomes the persistent
volume. Moving `/persist` off FAT32 changes the boot path, the recovery
fence of ADR 0178, the installer and `disk-update`, and needs its own
review.

## Decision

**Scope.** ext4 is read-write on additional volumes under `/mnt` and only
when the administrator asks. The persistent volume, `/persist` and its
aliases stay on FAT32. 0.3.0 ships opt-in read-write additional volumes,
not a persistent ext4 system volume.

**Mount mode.** A mount is read-only unless it asks for `rw`: the shell
takes `mount [-t TYPE] [-o ro|rw] SOURCE PATH`, and a configuration entry
`system.mount.N` takes an optional fourth field, `TYPE SOURCE PATH rw`. The
mount command travels to PID 1 as its own command kind, so an older
read-only mount message cannot become writable; the shell's control
protocol `GCTL0001` moves to version 5 for it. The static driver registry
marks ext4 `Writable`; PID 1 refuses `rw` for a driver without that mark
before any grant.

**Authority transition.** PID 1 first probes through a read-only endpoint,
as ADR 0222 describes. For a writable mount it then asks the block service
for a writable grant on the same slot, passing the identity the probe
found. The block service closes the probe endpoint as it installs the new
one, refuses a partition that overlaps another slot or the persistent
volume, and refuses one whose identity changed. PID 1 checks the returned
identity again and moves the writable end to the driver unused. A grant is
never widened in place: write authority always arrives on a new endpoint.

**Mode follows the slice.** The block service's `Info` reply now says
whether the client's slice is writable, in a byte version 2 required to be
zero, so `GBLK001` moves to version 3. The ext4 service mounts read-write
on a writable slice and read-only otherwise, so the service cannot hold
more authority than its mount uses, and a mount option cannot make a
read-only slice writable.

**Recovery.** A writable mount replays the journal and finishes the orphan
list before the service answers the VFS (ADR 0225). A read-only mount of a
volume that needs recovery is still refused; mounting it `rw` once
recovers it. Clean read-only mounts never replay.

**Replacement.** When a writable instance dies, PID 1 probes again, checks
type and identity, and grants a new writable endpoint before the
replacement starts; the grant closes whatever endpoint the slot had, so a
process that outlived its kill cannot write. The replacement replays the
journal before it answers. Open handles on the volume stay revoked, as ADR
0222 requires; callers reopen after the replacement attaches.

An instance also ends when the storage under it goes. When the VFS, the
block service or the driver exits, PID 1 kills every additional instance:
its VFS channel or block endpoint is dead, or the old driver dropped writes
the journal counted on. The replacements wait for the storage to come back
and then run the path above. These ends charge neither restart budget,
the slot's own or the supervisor's. PID 1 marks each instance it kills, so
the slot records its exit without counting it and the supervisor's count of
it is taken back once the exit is processed. An instance that saw the
storage go and exited first was counted already; the storage loss resets
both counts for every mounted slot, as the session services' budgets reset
after a storage fault. An instance that keeps failing with the storage up
still goes offline after three restarts. A writable instance whose write or
flush fails exits as well, so that a replacement replays the journal rather
than the failed instance answering errors. After a driver replacement the
block service describes the device again before it answers an additional
grant.

**Renames name their source.** `GFSPRT01` version 14 adds two filesystem
operations that carry the source entry, and the clean unmount below: `OperationRenameEntry` and `OperationReplaceEntry` put the moving
node in `Node`, the source directory in `Offset`, the source name at the
start of the request's cache slot, and the target as before. The VFS sends
them to any backend with `EntryBackend`, after resolving the source path's
parent within the same mount, and falls back to `RenameName` and
`ReplaceName` when the service answers `StatusUnsupported`. The remote
backend then stops trying entry renames for that service. FAT32 keeps its
node-only renames. The ext4 adapter copies the source name out of the
shared cache, checks that it still names the moving node with the same
generation, and refuses a node-only rename as unsupported. A replace onto
another name of the same inode succeeds and changes nothing, as rename(2)
does; the VFS answers a replace of a file by itself the same way.

**Unmount.** `OperationUnmount` asks a service to leave its volume clean
and answer every later request with an I/O error. The VFS detach checks
first that no file or scope holds the volume, then sends it, then removes
the mount. A service without a clean state answers it with a flush. If the
unmount fails, the VFS keeps the mount offline and returns the error; the
next detach removes it without claiming a clean volume, and the journal
stays for the next writable mount.

**Adapter.** New files get mode 0644, new directories 0755, owner and group
0; GoOS grants nothing from them (ADR 0223). Unlink removes a file, link or
special inode, or an empty directory. Setting the read-only attribute
clears the three write bits; clearing it restores the owner's. The
modification time goes to `i_mtime`. Acquiring an object pins its inode, so
a file unlinked while open keeps its blocks, stays readable and writable
through the handle, and is released at the last release or at unmount.
A write the engine splits over transactions returns the committed prefix.

**Durability.** Each mutation is durable in the journal when the service
replies (ADR 0225). `fsync`, `fdatasync` and a directory `fsync` reach the
service as a flush, which checkpoints the log and so only shortens the next
replay. An I/O error is an unknown outcome, as a lost reply is.

## What stays out

ext4 on the persistent slot; read-write 1 or 2 KiB block volumes; writing
extended attributes; owner and full mode changes from GoOS; a second disk;
more than four additional slots; a volume another writer, Linux or a
second GoOS, has open at the same time; and media identity stronger than
the GPT and MBR identity ADR 0222 records. A cloned disk with the same
GUIDs and UUID still passes the identity check, and the journal, not the
identity, decides what the volume holds.

## Evidence

- `go test ./userspace/ext4vol`: create, write, links, symbolic links,
  entry renames that move one of two hard links, replace onto a file and
  refusal onto a directory, unlink of a held file read through its handle,
  metadata changes, truncate, rmdir, a clean unmount that the read-only
  mount accepts, and a dirty volume refused read-only and recovered by the
  writable mount.
- `go test ./userspace/vfs`: the same operations through the VFS against a
  writable ext4 service, detach refused while a file is open, a clean
  volume after detach, and a failed clean unmount that leaves the mount
  offline until the next detach.
- `userspace/fsproto`, `userspace/block`, `userspace/exec`,
  `userspace/volume`, `userspace/fsdetect` and `tools/diskcompose` cover the
  wire changes, the writable `Info` flag, the writable mount command, the
  mount mode and the driver check, the restart budget a storage loss
  renews, and partition extraction. Each of the three protocols refuses a
  message of its previous version.
- `make test-ext4-rw` boots a disk with the installed system and three
  Linux ext4 partitions. It mounts one read-write from the configuration
  and one from the shell, writes, renames and unlinks through the shell,
  checks `mount` and `df`, refuses `rw` on a 1 KiB volume and an unknown
  mode, kills the configured instance and reads back what it wrote from
  the replacement, and runs `abi-probe ext4-rw`: a rename onto another
  link of the same inode, a rename to a free name, a file written after
  its last name went, and file and directory `fsync`. It then restarts the
  VFS, the block service and the driver in turn, reads what each volume
  held, writes a new marker after each, and for the block service and the
  driver holds a descriptor across the restart that must fail while a new
  open succeeds. Three more crashes of the configured instance then each
  recover, which the restart budget allows only if the storage restarts
  charged nothing. Last it unmounts the shell volume, mounts it read-only,
  and cuts the guest off with the configured volume mounted.
  `test-all` and `release-check` run it. On the host,
  the GoOS reader mounts the unmounted partition read-only, GoOS recovery
  reads the cut one, and the pinned `e2fsck -fn` passes the first while
  `e2fsck -fy` only replays the journal of the second before `-fn` passes
  it too; `debugfs` reads the files the guest wrote.

## Consequences

An administrator can write a Linux ext4 partition from GoOS and hand it
back to Linux, and a crash of the service or the machine leaves a volume
both systems recover. Each mutation costs two device flushes and one IPC
round trip; the release performance work measures that before batching.
The persistent volume keeps FAT32 until a separate decision moves it.
