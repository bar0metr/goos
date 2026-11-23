# ADR 0232: ext4 bounded parallel read execution

## Status

Accepted. Defines the M3 concurrency contract before any worker is introduced.
Implementation follows this contract; the KVM performance gate is recorded in
docs/development/ext4-validation.md.

## Context

One ext4 service executes one filesystem request at a time (spec §2.7). The
loop is `IPCReceive` → `fsproto.Serve` → `IPCSend`, all on one thread. This
serializes request handling, metadata-cache use, and block-client use, and
caps aggregate throughput on a read-only workload whose requests are
independent.

## Audit

Shared state under `userspace/`, with the concurrency contract each item
needs once readers run in parallel:

| Item | Owner today | After M3 |
|---|---|---|
| `cmd/ext4` `wire`, `requestMessage`, `replyMessage` | process | owner only: receive, decode, reply stay on the owner |
| `cmd/ext4` `requestedName`, `listedName` | process | per-request scratch; a read worker decodes into its own copy |
| `cmd/ext4` `blockWire`, `correlation` | process | per-worker: each worker needs its own correlation counter and request wire to overlap block IPC |
| `cmd/ext4` `bulkMemory`, `bulkObject` | process | owner only: bulk transfer is serialized; workers use inline reads |
| `cmd/ext4` `cache` (file cache) | process, shared with VFS | read data slots are request-named, so concurrent reads to different slots are safe; the single readdir name page is serialized |
| `cmd/ext4` `volume` | process | shared FS; read path must become concurrent-safe (below) |
| `ext4.FS.cache` (`blockCache`) | single owner | per-set lock (64 sets) plus atomic `clock`/`reads`/`hits`; a read copies the block it needs under the set lock |
| `ext4.FS.cache.overlay`, `.base` | mutation owner | written only under the mutation barrier; read concurrently after the barrier orders the writes |
| `ext4.FS.list` (`ListPrefetch`) | single owner | readdir path stays serialized in this milestone |
| `ext4.FS.scratch` | single owner | per-call, moves to a stack/request buffer |
| `ext4vol.Volume.prefetch` (`dirPrefetch`) | single owner | readdir path stays serialized |
| `ext4vol.Volume.entry/source/target` | single owner | per-call |
| `cmd/block` request wire, `correlation`, backend | process | must pipeline: overlap driver I/O across in-flight requests |

The audit above is added to docs/development/native-concurrency-audit.md and
the machine-readable native-concurrency-audit.tsv once the worker fields
exist; the reclassification happens in the same change that adds them.

## Decision

### Runtime

The ext4 service adopts the existing goroutine runtime the VFS uses
(ADR 0204): a `goos_goroutines` build variant, `goroutine.Init` plus
`arena.InitService` at entry, and a serial `!goos_goroutines` build that keeps
the current loop for boot checks. The owner is the main goroutine.

### Worker model

```text
owner (main goroutine)
    +-- read worker 0..MaxReadWorkers-1   (bounded, goroutines)
    +-- mutation owner == owner           (serialized barrier)
```

The owner receives and classifies every request. Read-only operations
(`Stat`, `Open`/lookup, `Read`, `ReadLink`, `FilesystemInfo`) are admitted to
the read pool. Mutations (`Create`, `Mkdir`, `Write`, `Truncate`, `Link`,
`Unlink`, `Rename`, `Replace`, `Symlink`, `SetMetadata`, `Acquire`,
`Release`, `Flush`, `Unmount`) run on the owner and are a barrier: a mutation
waits for in-flight reads to retire and blocks new read admission until it
finishes, so the mutation overlay and base maps are single-writer and reads
never overlap a map write. `Acquire`/`Release` stay on the owner because they
pin inodes on a writable volume.

### Metadata cache

`blockCache.get` keeps its set-associative layout. The 64 sets get one lock
each; `clock`, `reads`, `hits` become atomic. A read copies the inode or
directory bytes it needs while it holds the set lock, so the returned slice is
owned, not borrowed; the shared block is never parsed after the lock drops.
`overlay` and `base` are only mutated by the owner under the mutation barrier.

The readdir prefetch (`ListPrefetch`) and the decoded `dirPrefetch` batch stay
on the owner for this milestone: directory listing already runs in bounded
batches (ADR 0231), and parallelizing it does not gate the 1/2/4/8-client
lookup/stat/read target.

### Block I/O

Each read worker owns a correlation counter and a request wire, so block
requests overlap. The block service is serial today: its `serve` waits for the
driver reply before the next receive, so overlapping ext4 requests still
serialize at the block service. The 2x target therefore also requires the
block service to pipeline driver I/O. That change is part of M3 and is
measured before the cache work is assumed sufficient; if pipelining proves
impossible within bounds, the profiling evidence and the remaining serial
boundary are recorded instead of faking concurrency.

### Bounds

`ext4.MaxReadWorkers = 4`, classified in the resource-policy inventory. The
read pool and its per-worker wires are fixed-size; nothing grows with the
request rate or the directory size.

## Consequences

Read-only lookup/stat/read requests overlap, and a dedicated 1/2/4/8-client
workload measures aggregate throughput. The target is at least 2x at four
clients over one, with no material single-client regression. Mutations remain
exactly one serialized owner, and journal/transaction/orphan/pin state is
never touched concurrently. The serial build stays as the boot-check baseline.
