# ADR 0187: Per-object locks for Linux families, handle tables, and channels

## Status

Accepted. Amends ADR 0184: the linux, handle, and channel domains each give
way to finer locks, and the level order changes with them.

## Context

ADR 0184 left three domains wide. One lock covered the Linux personality's
state, one every handle table, and one every channel. ADR 0184 named them as
the next locks to split once a profile showed they mattered.

`make profile-locks` boots a diagnostic guest on four processors, runs five
workloads, and prints per lock level the acquisitions, the ones that
waited, their total and longest spin, and the kernel peaks
(`tests/boot/lock_profile.sh`). The workloads:

- `smp`: the probe's scheduler and futex checks;
- `kernel`: four threads and two processes making calls that share no state;
- `domains`: three copies of the probe's `domains` mode at once, each with
  two file threads, a pipe thread, and a fork thread;
- `copies`: two native `cat` copies of one file at once;
- `7zip`: 7-Zip archiving `/system/linux`.

Before this change, the share of acquisitions that waited was:

| Workload | linux | handle | channel |
| --- | ---: | ---: | ---: |
| smp | 6.1% | 13.4% | 2.1% |
| kernel | 37.5% | 7.1% | 5.1% |
| domains | 45.5% | 61.1% | 1.8% |
| copies | 0% | 65.9% | 1.3% |
| 7zip | 0.7% | 15.0% | 2.4% |

In `copies` the handle lock alone spent 5.8·10¹⁰ cycles waiting. In
`kernel` the fast Linux calls took the linux lock twice each to find their
state and deliver signals, so threads that shared nothing waited on each
other. Each split below moved contention to the next wide lock: with the
handle tables split, channel waits in `copies` rose to 58.8%; with the linux
lock split, to 59.9%. The profile after each step is in the commit that made
it; the numbers here are from the same workloads on QEMU TCG.

## Decision

### Handle tables

Each process's handle table has a reader-writer lock of its own at the
handle level, ranked by process ID. A call takes its own table in its set.
A call that moves handles to another process takes both tables, the lower
process ID first (`heldLocks.takeTables`). A process ID is stable for the
process's life, so two processors that need the same pair order it alike;
nothing orders locks by address.

A call learns that it needs a second table only once it has read its
message. It then asks for the table and returns before it changes anything;
the dispatcher releases the set and runs the call again with both tables
(`peerWanted`). A native call runs again once; a peer that changes again is
an `ActionRetry`. A Linux call keeps the last peer per task as a hint, so
its next file call takes the VFS service's table at once.

Finding the process that holds the other side of a channel used to scan
every table. A holder index now maps each channel endpoint to the processes
that hold it (`kernel/handle/index.go`). Tables update it as handles come
and go, under a leaf lock above every table.

### Linux families

A family is the processes one launch forked. They share descriptors, a VFS
session, and a thread group's signals; no call reaches into another
family's state. The linux level now holds 64 family locks, and a family takes
the one its ID hashes to. Two families may share a bucket, which costs only
concurrency.

The personality's shared tables (process states and records, threads, open
descriptions, directories, VFS sessions, and the IDs they hand out) lock
themselves with `tablesLock` at the linux-tables level, one table operation
at a time (`kernel/compat/linux/guard.go`). Exported functions take it;
their `...Held` forms run under it.

Fast calls take no Linux lock. Each task caches its state, thread ID, and
family (`linuxCallers`). A fast call takes its family's lock after the call
only when a signal is pending, which it reads from an atomic word
(`Signals.AnyPending`). File and process calls hold their family's lock for
the whole call, so a family's threads still run their file calls one at a
time.

### Channels

The channel registry locks itself with a short slots lock, held only to
find, add, or remove a channel (`ipc.Registry`). Each channel has its own
lock for its queues, outstanding requests, and open sides. A call holds a
channel's lock only for one operation on it: `Acquire` finds the channel
and takes its lock under the slots lock; the caller unlocks it when done.
No call holds two channels, so channels need no order among themselves.

The rules that keep messages and wakes right:

- A side closes once. `Registry.Close` reports which call closed it, and only
  that call wakes the peer with `PeerDied`.
- The call that closes the second side removes the slot under the slots
  lock and takes the channel's lock before it frees the memory, so a caller
  that found the channel finishes first.
- A sender queues under the channel's lock and wakes waiters after it. A
  receiver registers its wait before it looks again. A message sent between
  the two is found by the second look or wakes the waiter.
- Queues stay FIFO, and a message is queued once, under one lock.
- Handles move inside the send, under the channel's lock and both tables.
  A transfer that fails rolls back the handles it already placed.

The table of executable page requests had lived under the channel lock. It
has its own paging lock now, below the channel locks. A request is recorded
and sent under it, and the reply takes it before it looks up the request, so
the reply always finds the record.

`IPCReceive` and `IPCWaitReceive` take the registry, their own table for
reading, and their address space. `IPCSend` no longer takes the stream lock
in its set. A send that moves a stream handle asks for the stream lock as
it asks for the peer's table. Before this, every send and receive held the
stream lock, and in `copies` it became the next bottleneck at 57%.

### Level order

| Level | Lock | Several at once |
| ---: | --- | --- |
| 1 | registry (reader-writer) | |
| 2 | linux family (64) | no |
| 3 | lifecycle | |
| 4 | handle table (reader-writer, one per process) | two, lower process ID first |
| 5 | stream | |
| 6 | shmem | |
| 7 | address space (reader-writer, one per process) | no |
| 8 | executable | |
| 9 | paging | |
| 10 | channel slots | |
| 11 | channel (one per channel) | no |
| 12 | futex bucket (32) | two, lower rank first |
| 13 | wait tables | no |
| 14 | linux tables | |
| 15 | handle index | |
| 16 | log | |
| 17 | memory | |
| 18 | random | |
| 19 | scheduler | |
| 20 | console | |

The channel locks moved above the object domains. A call takes them after
the set it started with, so they rank above every lock a set holds.

### Locks left wide

- The registry stays one reader-writer lock. Lookups read it; only process
  creation and teardown write it. At most 8% of its acquisitions waited.
  Its spin totals include the `diag` dump, which holds it while it prints
  each process.
- The stream lock stays one lock. After the IPC sets stopped taking it, at
  most 7.8% of its acquisitions waited.
- The shmem lock stays one lock. `copies` waits on it 14.5% of the time:
  each native send holds it in case the message answers an executable page
  request. That costs less than one more lock order to maintain.
- A family's threads share one lock for file and process calls. In
  `domains`, 28.8% of family lock acquisitions waited: each copy runs four
  threads of one family. Splitting a family's descriptor table from its
  signals would let them overlap, at the cost of a second lock in every
  file call.
- The VFS service's handle table is the peer of every Linux file call that
  lends it a buffer. In `domains` this keeps the handle level at 33%
  contended. Taking the service's table for reading would need the holder
  index and the table's slots to change without its write lock.

### Checks

Validation builds still stop on a lock taken out of level order, a second
lock of a level taken out of rank order, a recursive acquisition, and a
`LOCK_STUCK` spin. Each channel checks that its caller holds its lock; each
handle table checks its own lock once its process has a task; the Linux
tables check `tablesLock`. No lock replaces the three that were split.

## Consequences

After the three splits, the same workloads waited this often:

| Workload | linux | handle | channel | channel slots | stream |
| --- | ---: | ---: | ---: | ---: | ---: |
| smp | 7.3% | 0% | 0.35% | 0.66% | 0.8% |
| kernel | 2.9% | 0% | 0.43% | 0.53% | 2.0% |
| domains | 28.8% | 33.5% | 0.01% | 0.02% | 7.8% |
| copies | | 0% | 0.09% | 2.7% | 0.5% |
| 7zip | 0.4% | 0.9% | 0.15% | 0.14% | 1.2% |

In `kernel` the linux level went from 288,499 acquisitions to 70, since
fast calls no longer take it. In `copies` the total spin of the hottest
lock fell from 5.8·10¹⁰ cycles on the handle lock to 5.4·10⁸ on shmem. The
kernel-parallel and kernel-thread peaks on four processors stay at 4.

`make test-race` runs the concurrency tests under the Go race detector:
file calls of two processes and of two threads, fork and exec beside file
calls, traffic on one channel while another's lock is held, a close racing
sends and receives, a release waiting for a lock holder, a handle close
racing lookups, transfers between two tables in both directions, and a
process exit racing transfers into its table.

A native send's message and a receive's buffer are passed by reference
(`SendFrom`, `ReceiveInto`). TinyGo gave `Receive`, which returns a message,
a 2 KiB frame, and helpers around it pushed a call past its 16 KiB kernel
stack.
