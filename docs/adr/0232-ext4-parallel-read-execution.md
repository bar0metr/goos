# ADR 0232: ext4 bounded parallel read execution

## Status

Accepted. Defines the final M3 concurrency contract. The validation record in
docs/development/ext4-validation.md names the tested commits and KVM runs.

## Context

Before M3, one ext4 service executed one filesystem request at a time
(spec §2.7). Its loop was `IPCReceive` → `fsproto.Serve` → `IPCSend`, all on one
thread. That baseline serialized request handling, metadata-cache use and
block-client use, and capped aggregate throughput on independent read-only
requests.

## Audit

The final M3 implementation uses these ownership boundaries:

| Item | Final owner / synchronization |
|---|---|
| `cmd/ext4` `wire`, `requestMessage`, `replyMessage` | owner receives, decodes and replies |
| `cmd/ext4` `requestedName`, `listedName` | owner scratch; admission copies `Request.Name` into the read job before handoff |
| `cmd/ext4` `blockHandle` | process-global, immutable after startup |
| `cmd/ext4` `blockTransport.next`, send/receive | one transport; its mutex protects the correlation sequence and complete request/reply exchange |
| `cmd/ext4` `blockClient.bulkMemory`, `bulkObject` | private to the checked-out client |
| `cmd/ext4` `pooledDevice.clients` | bounded ownership pool; all clients share the serialized transport |
| `cmd/ext4` `cache` (file cache) | workers use request-named data slots; owner serializes the readdir name page |
| `cmd/ext4` `volume` | shared FS; read workers use concurrent-safe read paths |
| `ext4.FS.cache` (`blockCache`) | one lock per set (64 sets); atomic `clock`/`reads`/`hits`; callers release borrows before another same-set access |
| `ext4.FS.cache.overlay`, `.base` | mutation owner writes behind the read barrier |
| `ext4.FS.list` (`ListPrefetch`), `ext4vol.Volume.prefetch` | owner serializes readdir |
| inode and volume decode scratch | per-call |
| `cmd/block`, virtio-blk request and driver state | serial service loops; no M3 pipeline |

The native-concurrency audit records the globals and these object-level owners.

## Decision

### Runtime

The ext4 service adopts the existing goroutine runtime the VFS uses
(ADR 0204): a `goos_goroutines` build variant, `goroutine.Init` plus
`arena.InitService` at entry, and a serial `!goos_goroutines` build that keeps
the current loop for boot checks. The owner is the main goroutine.

### Worker model

```text
owner (main goroutine)
    +-- read worker 0..maxReadWorkers-1   (bounded, goroutines)
    +-- mutation owner == owner           (serialized barrier)
```

The owner receives and classifies every request. Read-only operations
(`Stat`, `Open`/lookup, `Read`, `FilesystemInfo`) are admitted to the read pool.
The owner handles `ReadLink` and directory listing. Mutations (`Create`, `Mkdir`,
`Write`, `Truncate`, `Link`,
`Unlink`, `Rename`, `Replace`, `Symlink`, `SetMetadata`, `Acquire`,
`Release`, `Flush`, `Unmount`) run on the owner and are a barrier: a mutation
waits for in-flight reads to retire and blocks new read admission until it
finishes, so the mutation overlay and base maps are single-writer and reads
never overlap a map write. `Acquire`/`Release` stay on the owner because they
pin inodes on a writable volume.

### Metadata cache

`blockCache.get` keeps its set-associative layout with one lock for each of 64
sets; `clock`, `reads` and `hits` use atomics. It lends a block until the caller
releases the set lock. Callers consume or copy bytes before that release and
release the borrow before another same-set lookup. Extent traversal copies a
node before descending; indirect traversal extracts the next pointer and
releases its borrow before reading that block. The owner alone mutates `overlay`
and `base` behind the read barrier.

The read workers share the owner's `FS`: a per-worker read-only mount is
rejected here, because a read-only mount refuses a volume the owner holds
read-write (the writable mount sets `incompatRecover`, so a second read-only
mount answers `KindNeedsRecovery`). Shared decode state with a synchronized
cache is the only path that sees the owner's committed and pending state.

The readdir prefetch (`ListPrefetch`) and the decoded `dirPrefetch` batch stay
on the owner for this milestone: directory listing already runs in bounded
batches (ADR 0231), and parallelizing it does not gate the 1/2/4/8-client
lookup/stat/read target.

### Block I/O

Up to four read-only fsproto requests may execute concurrently. Each checked-out
`blockClient` owns its bulk buffer, but all clients share one `blockTransport`.
The transport mutex covers encoding, send, receive, decoding and correlation
validation. It owns one monotonically increasing correlation sequence and permits
one outstanding request on `blockHandle`. No concurrent caller receives from that
endpoint. The block service and virtio-blk driver also retain serial request loops.

### Block pipeline experiment: attempted and reverted

The experiment added a bounded block-service worker pool and four virtio-blk DMA
slots, each with its own descriptor chain and status byte. A driver collector
matched completions by correlation. Bulk transfers retained one owner.

The earlier ext4-only run `20261123T155827Z-52b02f0b` (commit `6bdc5916`)
measured 882 ops/s at one client and 1225 at four (1.39x), below 2x.

The pipeline was implemented and measured, then reverted. The block service
gained a bounded read-worker pool (commit `0b1a16c3`) and the driver a four-slot
non-blocking submit path with a collector goroutine (commit `30154b16`); both
ran on the goroutine runtime. KVM run `20261123T173540Z-362fc2f7` booted and
passed, but throughput fell to 725 ops/s at one client and 1083 at four, and
readdir-10k rose from 221 ms to 300 ms. The experiment lost throughput; those
measurements do not isolate runtime park/awake, channel handoff or driver costs.
The final implementation keeps the serial block service and driver.

The missed 2x is attributed to more than one serialization point, not the
virtio-blk round-trip alone. The benchmark issues an open, a 4 KiB read, an
fstat and a close each iteration, and the VFS read pool schedules the
additional-volume `Remote` backend as `ReadLanePersistent`, which permits only
one persistent-lane read at a time; concurrent file-data reads can therefore
serialize in VFS before they reach ext4. Both the VFS persistent read lane and
the serial block-service/driver request path remain serial boundaries, and
neither was measured in isolation. This milestone retains those boundaries;
FIX3 adds acceptance gates without reopening the pipeline experiment.

### Transport experiment: attempted and reverted

The correctness fix replaced per-client endpoint receives with the serialized
transport described above. The first attempt used a receiver-goroutine
multiplexer and measured a 14x read-only and 4x readdir regression on the
goroutine runtime; the implementation reverted that experiment. The accepted
serialized transport avoids that cross-goroutine handoff. It remains a serial
boundary. The 217 ms readdir-10k result returns near the earlier M3 pre-FIX
level, not the M2 post-FIX result of 184 ms.

### Bounds

`userspace/cmd/ext4.maxReadWorkers = 4` appears in the resource-policy inventory.
The service starts four read-worker goroutines. Both `jobs` and `results` have
capacity `maxReadWorkers`, and admission keeps in-flight reads at or below four.
The client ownership pool also has a fixed bound. Request rate cannot grow these
resources.

## Consequences

Read-only lookup/stat/read requests overlap, and a dedicated 1/2/4/8-client
workload measures aggregate throughput. The target is at least 2x at four
clients over one, with no material single-client regression. Before FIX3, the
serialized-transport run measured 1.34x and about 7.2% less single-client throughput
than the pre-M3 result; the validation record retains both regressions and the
performance fallback. Final FIX3 run `20261124T071705Z-2668fdcb` on clean
`b85957ed` passed correctness and cleanup, measured 1.39x at four clients, and
retained a 7.5% single-client loss from pre-M3. Readdir-10k took 223.555 ms,
21.5% above the M2 post-FIX 184 ms result. The final measurements still miss
the performance goals; the validation record names the accepted fallback and
unprofiled serial boundaries. Mutations retain one serialized owner; readers do not
concurrently mutate journal/transaction/orphan/pin state. The serial build stays
as the boot-check baseline.
