# ADR 0057: Launch-path costs and the latency baseline

## Status

Accepted.

## Context

Before this work a single-process command took about 160 ms at 100 Hz and a
persistent write close to two seconds. ADR 0049 had attributed about half a
second to process launch, independent of the tick rate. With the in-guest clock
and launch-stage profile (ADR 0054), the chain could be measured instead of
inferred.

Two causes accounted for nearly all of it.

- **Waiting for ticks.** Four kernel tasks halted in turn, IPC hops and exit
  delivery waited for the next tick, and a VFS round trip cost four ticks.
  ADR 0055 removed these waits.
- **Loading the image.** It took 13.4 ms per launch, against about 0.3 ms for
  every other kernel stage combined. Timing the phases inside `LoadNative`
  showed that almost all of it went to initializing pages. Each data segment
  stored its stack and heap reservation as file bytes, so a 54 KiB utility was
  a 215 KiB image. Every page was then cleared and filled with Go byte loops,
  and the page allocator tested one bitmap bit at a time with a 64-bit modulo.
  Under TCG, byte-granular memory operations cost tens of nanoseconds per byte.

## Decision

- `elf2native` drops trailing zero bytes from each segment's file data. The
  loader already zero-fills memory past the file bytes, so behaviour is
  unchanged while images shrink: coreutils from 215 KiB to 54 KiB, VFS from
  484 KiB to 177 KiB, and the block service from 174 KiB to 10 KiB.
- Physical page initialization, user copy-in and copy-out, and page-table
  clearing use `rep movsq`/`rep stosq` with a byte tail, through two small
  assembly routines.
- `pmm.Allocator` skips bitmap words with no free page and locates the first
  free bit with a trailing-zero count, in both single and contiguous
  allocation. On the host, allocation across a 40,000-page used run fell from
  30 µs to 0.7 µs.

## Measurements

QEMU 11.0.1 TCG on arm64, 10 runs per case, median in microseconds. The
baseline is `main` before ADRs 0055–0057. "Scheduler" adds safe-return
rescheduling and SSE state switching. "Loader" adds the changes above.

| case | 100 Hz baseline | 100 Hz scheduler | 100 Hz loader | 250 Hz loader | 1 kHz baseline | 1 kHz loader |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `uname` | 160,840 | 15,660 | 4,768 | 4,909 | 26,484 | 3,862 |
| `echo x` | 161,089 | 14,692 | 4,800 | 4,925 | 25,274 | 3,838 |
| `ls /system` | 290,678 | 23,145 | 9,075 | 9,076 | 55,724 | 7,730 |
| `cat /system/motd` | 330,523 | 23,566 | 9,147 | 9,215 | 59,935 | 7,928 |
| `cat /persist/latency.txt` | 331,050 | 24,200 | 9,072 | 9,297 | 60,442 | 8,016 |
| `echo y > /persist/latency.txt` | 1,896,857 | 50,659 | 20,985 | 21,951 | 236,601 | 17,170 |
| `echo a \| readline \| readline` | 359,975 | 44,936 | 12,312 | 12,899 | 70,633 | 10,221 |
| launch failure (`missing-stage`) | 79,598 | 1,023 | 844 | 882 | 8,340 | 773 |
| VFS request/reply (`bench vfs`) | 40,111 | 156 | 152 | 165 | 4,122 | 135 |
| VFS → block → driver read (`bench read`) | 161,647 | 609 | 583 | 634 | 16,367 | 513 |

Kernel launch stages at 100 Hz, average per launch in microseconds:

| stage | before loader changes | after |
| --- | ---: | ---: |
| image lookup | 83 | 78 |
| address space | 15 | 13 |
| image load | 13,449 | 1,766 |
| process setup | 152 | 152 |
| task install | 30 | 8 |

Most of the remaining image-load time is spent clearing the new process's
stack and heap pages.

A traced `uname` at 100 Hz, measured from the exec request reaching PID 1:

| step | time |
| --- | ---: |
| PID 1 builds the launch request; the kernel wakes and completes the spawn | 2.45 ms |
| the launch reply reaches PID 1, which starts waiting on the process | 0.3 ms |
| the utility starts, prints, and exits | 1.8 ms |
| exit delivery to PID 1 and the exec reply to the shell | 0.5 ms |

## Consequences

- Command latency no longer depends strongly on the tick rate. It is within
  3% between 100 Hz and 250 Hz. At 1 kHz it is about 20% lower, because the
  housekeeping loop and preemption run more often.
- The half-second launch cost no longer exists.
- A housekeeping step holds interrupts off while it loads an image, about
  2 ms. At 1 kHz that coalesces about two ticks per launch. Tick-based
  deadlines therefore stretch slightly during launches; the TSC clock is not
  affected.

The next costs, in order, are:

1. clearing fresh stack and heap pages, which demand-zero mapping would defer;
2. utility startup in TinyGo's size-optimized code;
3. the extra context switch that safe-return rescheduling adds to each
   request/reply exchange.
