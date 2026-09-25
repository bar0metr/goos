# ADR 0096: FAT32 as a separate filesystem service

## Status

Accepted. Amends ADR 0036, ADR 0071, ADR 0077 and ADR 0078.

## Context

The FAT32 filesystem ran inside the VFS process (ADR 0036). The kernel stayed
out of it, but two security domains shared one address space: the namespace,
which holds every session, every open file and the path policy, and a parser
of an untrusted on-disk format. A malformed volume or a FAT32 bug could take
over the process that decides what every other process may reach. The VFS also
held the block client endpoint, so a compromised namespace reached the whole
disk directly, and a crash in the parser took the whole namespace down,
`/system` and `/tmp` included.

The specification separates the VFS, which owns namespaces, mounts and path
resolution, from filesystem implementations, which own their format.

## Decision

**FAT32 is its own process, `/system/fat32`.** It holds three capabilities: the
console (write), the server end of a channel to the VFS, and the client end of
a channel to the block service. It has no namespace, no session, no image, and
no device authority. It mounts the volume at startup; a volume it refuses
stays refused, and the service keeps answering with I/O errors rather than
exiting into a restart loop (ADR 0077).

**The VFS reaches it through a typed protocol.** `userspace/fsproto`
(protocol `GFSPRT01`, version 1) carries exactly the namespace backend
operations — open, create, mkdir, read, write, truncate, stat, readdir,
rename, unlink — plus the volume root and flush, over node IDs and single name
components. Requests and replies are one bounded IPC payload each: up to 216
bytes of data per write and 224 per read, names up to 63 bytes, reserved bytes
zero, every length checked before use. `fsproto.Serve` is the one dispatcher
from a request to a volume, shared by the service and the host tests.

**In the VFS the volume is a remote backend.** `vfsservice.Remote` implements
the backend interface by exchanging messages; the namespace, the open-file
table and the mounts are unchanged. Every exchange has a deadline
(`fsproto.RequestDeadline`, six seconds, above the block protocol's own). A
timeout, a malformed or mismatched reply, or a dead peer is `StatusIOError`,
which the namespace reports as a backend error, on the persistent mounts only.
A peer that has died is forgotten at once, so later calls fail without
waiting. Replies to requests that timed out are discarded by correlation.

**PID 1 connects the two after the service starts.** The VFS no longer mounts
`/persist` at startup. PID 1 launches `fat32` after the block service, then
hands the VFS its end of the channel through a new VFS control operation,
`SetFilesystem` (VFS control version 4), and waits for the VFS to mount the
volume before starting the services that use `/state`.

**The service restarts on failure.** Its node IDs name places on the volume
(directory cluster and slot), so a restarted instance serves the same IDs. On
restart PID 1 creates a fresh channel to the VFS and a fresh one to the block
service, hands the block service its end through a new block control
operation, `SetFilesystem` (block control version 2), and hands the VFS its end
as at first start. The VFS checks that the new instance serves the same root
and keeps its mounts and open files: a handle opened before the crash reads
through the restarted service. The block service drops a filesystem endpoint
whose peer has died instead of waking on it forever.

## Alternatives

**Keeping FAT32 in the VFS with more validation.** Mount-time validation
already exists (ADR 0077). It narrows the bugs a malformed volume can reach;
it does not separate what a remaining bug can take over.

**A shared-memory page cache between the VFS and FAT32.** It is the target of
the performance architecture, and it would remove the extra copy per chunk.
It needs mapping lifetimes and revocation that do not exist yet; the protocol
leaves room for it, since reads and writes already name node and offset.

**Restart-never for the new service.** Simpler, but a crash would lose
persistent storage until reboot, while the driver below it already recovers
(ADR 0078).

## Consequences

The VFS holds no block endpoint and no filesystem code for the persistent
volume; `userspace/vfs` does not import `userspace/fat32`. A compromised VFS
reaches the disk only through the FAT32 protocol, and a compromised FAT32
service reaches the volume but not the namespace, sessions, or other mounts.

Each persistent operation costs one more IPC round trip, and data moves in
chunks of at most 224 bytes. The validation boot test kills the service
(`fault fat32`) under a running system and requires `/tmp` and the shell to
keep working, the block service and the VFS to take the new endpoints, and a
file on `/home` to be read again through the restarted service.

Host tests cover the protocol's encoding and rejection of damaged messages,
the dispatcher against a volume that misreports, and in the VFS: service
death, malformed replies, a restarted service with a different volume, large
transfers across several exchanges, and handles surviving a restart.

What is not recovered: a write in flight when the service dies is reported as
an I/O error and may be partly applied, as it would be on a power cut; FAT32
has no journal. The VFS itself still restarts never, since every session and
open file lives in it.
