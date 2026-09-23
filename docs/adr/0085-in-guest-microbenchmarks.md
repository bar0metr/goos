# ADR 0085: In-guest microbenchmarks

## Status

Accepted. Extends ADR 0054.

## Context

The specification asks for microbenchmarks of the context switch, IPC
ping-pong and throughput, shared-memory throughput, page allocation, page
faults, cached and uncached VFS reads, and RAMFS metadata operations, with the
results in `build/reports/`, and it forbids optimising on assumptions.

`make test-performance` ran only host `go test -bench` benchmarks of kernel
data structures. They time an algorithm in one host process: no trap, no
address-space switch, no second process, no scheduler. A context switch, an
IPC exchange between processes, or a read through the storage stack exists
only in the running system. `make test-latency` (ADR 0054) already boots a
guest, but it times whole command lines and two VFS loops.

The native ABI limits what an ordinary process can set up. Streams may be
created by any process, but channels only by PID 1, shared-memory objects only
by PID 1 and the kernel, and a process cannot start another one. There is no
yield call, and a user page fault terminates the task: there is no demand
paging.

## Decision

**Measure in the guest with the `bench` command and existing primitives.** No
kernel operation is added. `bench` gains four modes:

- `bench stream COUNT SIZE` creates a private stream and, per operation,
  writes SIZE bytes, reads them back, performs the read that finds the stream
  empty, and reads the clock. It is the same work as one pipeline chunk
  without a wait and without a switch.
- `bench send COUNT SIZE | bench recv COUNT SIZE` runs two processes connected
  by the shell's pipeline stream. The sender sleeps 0.1 s so the receiver is
  blocked on the empty stream, then writes. The receiver times from its first
  chunk to its last, counts its waits, and reads the clock after every chunk;
  a chunk preceded by exactly one wait is one round trip between the
  processes, and the median of those intervals is reported with the average.
- `bench meta PATH COUNT` creates and unlinks PATH through the VFS.
- `bench read` now also reports the file size and rate.

`tests/boot/microbench.sh`, run by `make test-microbench`, boots the normal
build, warms each case up once, runs it `MICROBENCH_REPEATS` times, and writes
`build/reports/microbench-hz<rate>.md`: the median over the runs of each value,
the lowest average, the derived context switch, and the raw lines.
`make test-performance` runs it before the host benchmarks.

What each specified benchmark maps to:

| Benchmark | Measurement |
| --- | --- |
| context switch | derived: (handoff round trip - one-process stream operation) / 2 |
| IPC ping-pong | `bench vfs`: channel request and reply between a client and the VFS service |
| IPC throughput | `bench send N SIZE \| bench recv N SIZE` |
| cached VFS read | `bench read` of a 10 KiB file of the memory-resident system image |
| uncached VFS read | the same bytes copied to FAT32; 20 sectors exceed the 4-sector cache |
| RAMFS metadata | `bench meta /tmp/bench.tmp` |
| page allocation | host benchmark only |
| shared-memory throughput | host benchmark only; deferred in the guest |
| page fault | deferred |

## Alternatives

**A true two-process channel ping-pong.** It needs two `bench` processes with a
channel between them. Only PID 1 may create channels, and the exec protocol
connects stages only by streams, so it needs either `ChannelCreate` for every
process or a new launch shape in PID 1 and the shell. Both widen the attack
surface for a benchmark. The VFS round trip is a real ping-pong on the same
kernel path, measured with the service's work included.

**Ping-pong over two streams.** A pipeline is a line: there is no way to give
the second stage a stream back to the first.

**A kernel yield or a benchmark operation.** A yield loop between two
processes is the classic context-switch benchmark, but the kernel has no user
yield and adding one only for measurement would be an operation without a
user. The stream handoff forces the same switch through the paths real
pipelines use.

**Letting the receiver block on a full stream instead of an empty one.** Tried:
the sender blocks, every read wakes it, and the pair alternates until a timer
tick lands while the sender is runnable; from then on they move a time slice
of data per switch. Blocking on an empty stream has the same failure, which is
why round trips are recognised per chunk by their single wait rather than
assumed for the whole run.

**Page faults.** A user page fault terminates the task; there is no fault that
is serviced and resumed. Timing a faulting command would measure launch and
teardown, and would need a deliberately faulting program in `/system/bin`
(the fault probe exists only in the validation build). The measurement belongs
with demand-zero pages, the first item on the performance roadmap.

**Shared memory between processes.** An ordinary process cannot create a
memory object or receive one except the startup page from PID 1. A
shared-memory throughput figure needs the same missing channel, and after the
mapping it is memory bandwidth. The host benchmark stays.

## Consequences

Measured with QEMU 11.0.1 TCG on an arm64 host, 5 runs per case, median over
the runs. Another QEMU guest was running on the host throughout, so the figures
carry noise of tens of percent between runs.

| Case | 100 Hz | 1000 Hz |
| --- | ---: | ---: |
| stream calls, one process, 1 B (op median) | 7.9 µs | 7.1 µs |
| stream handoff round trip, 1 B (op median) | 48.6 µs | 44.4 µs |
| **context switch (derived)** | **20.3 µs** | **18.7 µs** |
| IPC ping-pong, VFS `Stat /` | 164 µs | 153 µs |
| IPC throughput, 1 B chunks | 79 KiB/s | 151 KiB/s |
| stream calls, one process, 256 B | 21,866 KiB/s | 26,304 KiB/s |
| IPC throughput, 256 B chunks | 6,771 KiB/s | 9,126 KiB/s |
| cached VFS read, 10 KiB | 8.4 ms (1,204 KiB/s) | 7.1 ms (1,419 KiB/s) |
| uncached VFS read, 10 KiB | 38.8 ms (259 KiB/s) | 32.9 ms (306 KiB/s) |
| RAMFS create + unlink | 444 µs | 377 µs |

What they show, without acting on it here:

- A switch between processes costs about 20 µs under TCG, the time of about
  ten native calls. A CR3 load also flushes the emulator's software TLB; how
  much of the 20 µs that accounts for is not measured, and on hardware the
  ratio will differ.
- A VFS request costs about eight switches' worth. A bare cross-process round
  trip is about 45 µs, so roughly 120 µs of each VFS request is spent in the
  client library, the service's wait and dispatch, and the protocol encoding.
  That is where to profile before changing the IPC path.
- Reads are bounded by the 224-byte VFS reply payload: a 10 KiB file takes
  about 50 requests (open, 47 reads, close), and the cached rate is the VFS
  request rate times the payload. The FAT32 path adds about 1.5 ms per
  512-byte sector, through the block service and the driver.
- A one-byte pipeline is faster at 1000 Hz: ticks break the alternation
  sooner, and more bytes then move per switch (30 waits for 4,999 chunks
  against 506 at 100 Hz).

Handoff runs at 1000 Hz keep about 20 of 199 chunks as round trips; the median
is stable, but fewer samples make it noisier. The guest TSC advances in steps of
about 1 µs on this host, which bounds the resolution of a single interval.

Setting up the uncached read exposed that `cat` and the redirection sink
`write` never rewound their arena between VFS requests, so either failed with
`UTILITY_OUT_OF_MEMORY` on a file of about 10 KiB. Both now rewind per chunk,
as `bench` does.
