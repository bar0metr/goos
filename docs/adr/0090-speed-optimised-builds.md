# ADR 0090: Speed-optimised builds by default

## Status

Accepted.

## Context

Every image was compiled with TinyGo's `-opt=z`, the smallest code. Nothing
recorded why; it was the setting the first kernel build used. The specification
treats performance as a real requirement and asks that decisions about it rest
on measurements (§13, §51, §61).

TinyGo accepts `-opt=0`, `1`, `2`, `s`, and `z`. There is no `-O3`: `2` is its
fastest level.

## Decision

**The default level is `2`.** `OPT` in the Makefile selects the level for the
kernel and every userspace image; `make OPT=z` still builds the small variant,
each level in its own `build/variants/` directory, and the latency and
microbenchmark reports carry the level in their names so two levels can be
compared.

Measured at 100 Hz under QEMU TCG on the same host, median of the harness runs
(`make test-microbench`, `make test-latency`), before the terminal-output change
of ADR 0089:

| Case | `-opt=z` | `-opt=2` | Change |
| --- | ---: | ---: | ---: |
| IPC ping-pong to the VFS service | 169.1 µs | 113.7 µs | −33% |
| Stream calls in one process, 256 B | 11.2 µs | 8.7 µs | −23% |
| IPC throughput between processes, 256 B | 52.3 µs | 31.7 µs | −39% |
| VFS read of the system image | 11.4 ms | 9.3 ms | −19% |
| VFS read of a FAT32 file | 43.4 ms | 47.2 ms | +9% |
| RAMFS create and unlink | 460.6 µs | 340.9 µs | −26% |
| `ls /system` | 5489 µs | 4172 µs | −24% |
| `echo a \| readline \| readline` | 3095 µs | 2507 µs | −19% |
| `uname` | 1719 µs | 1550 µs | −10% |

The FAT32 read is dominated by the emulated disk and within the run-to-run
spread; every path that runs GoOS code got faster.

## Alternatives

**Keep `z`.** It is about a quarter smaller: the kernel is 272 KiB instead of
349 KiB and the boot archive 1.6 MiB instead of 2.1 MiB. Both fit their bounds
at either level (the archive's is 4 MiB, ADR 0087), and the size buys nothing
the system uses, while IPC, the central mechanism, is a third slower.

**`s`.** A middle ground in size; it was not measured, and nothing needs the
bytes it would save.

**Choosing per component.** The kernel at one level and userspace at another,
for example. It adds a second knob for no measured benefit.

## Consequences

The boot archive is about 2.1 MiB. That crosses the old two MiB image window,
which is how the ADR 0087 addendum found that the window had not grown with the
archive bound; the window is four MiB now.

Different code generation changes timing, and the first `-opt=2` boot test
exposed a race in which a job notice arriving with a typed line dropped the
prompt; ADR 0089's terminal service redraws it.

Stack frames change too. The frame bound of ADR 0088 is checked on every build
at whatever level is chosen; at `-opt=2` the largest userspace frame is about
9.4 KiB against the 65400-byte bound and the largest kernel frame on a guarded
stack about 2.9 KiB against 8184 bytes.

The reproducibility check (ADR 0086) covers the level like any other build
setting: `build/config/build` records it.
