# ADR 0219: Bounded load randomization for Linux static PIE

## Status

Accepted. Supersedes the fixed base of ADR 0149. Draws from the generator
of ADR 0185.

## Context

ADR 0149 loaded every Linux static PIE at `0x40000000`. A program that
printed its own addresses printed the same ones on every run, on every
machine, so an attacker who knew the binary knew where its code, data and
GOT sat. ADR 0149 kept the base fixed for two reasons: a CPU without RDRAND
had no entropy to draw from at launch (ADR 0151), and a layout that changed
with the CPU would have made the Linux boot test depend on its machine.

ADR 0185 removed the first reason. The kernel generator seeds from timing
jitter, with RDRAND as an extra source when the CPU has it, and serves
every CPU. The second reason only needs a test that checks properties of
the base instead of its value.

## Decision

**Scope.** The kernel randomizes the base of a Linux ELF64 x86_64 `ET_DYN`
image without `PT_INTERP`, a static PIE. An `ET_EXEC` image loads at its
linked addresses, as before. Dynamic linking stays unsupported.

**Window.** `kernel/compat/linux/elf` replaces `StaticPIEBase` with
`StaticPIEWindowStart`, 1 GiB (`0x40000000`), and `StaticPIEWindowEnd`,
`vm.UserImageAreaLimit`, 2 GiB (`0x80000000`). `GoOSConfig(pieBase)` takes
the base the loader chose.

**Legal bases.** A base is legal when:

- it is a multiple of the image's largest alignment: the page size, every
  `PT_LOAD` `p_align`, and the alignments of `PT_PHDR`, `PT_TLS`,
  `PT_GNU_RELRO` and `PT_DYNAMIC`;
- it is at least the window start;
- the base plus the relocated image end plus `BrkReserve` (64 MiB) is at
  most the window end.

The brk area, from the page-aligned image end for 64 MiB, therefore always
fits inside the window. The brk base and the 64 MiB brk contract do not
change.

**Selection.** The loader parses the image once at the window start with
the ordinary parser (`linuxelf.ParseInto`) to learn its span and its
alignment. `linuxelf.PlacePIE` turns the legal bases into an arithmetic
sequence: the first base, a step equal to the alignment, and a count. The
loader draws one 64-bit value `r` from the ADR 0185 generator and picks slot
`(r * count) >> 64`, the high half of a 64x64->128 multiply; the bias toward
any slot is at most `count / 2^64`. It then parses the image again at the
chosen base, so every hostile-ELF check (user range, page congruence,
overlap, W^X, executable entry, wrap) runs on the final addresses. The
placement takes bounded time: one draw, two parses, no retry loop, and no
second parser.

**When.** A PID 1 launch and every successful `execve` place the new image
at a fresh base. `fork` and thread creation keep the current layout, so a
child shares its parent's base.

**Failure.** When the generator refuses output, the launch fails; there is
no fallback to a fixed base. The generator needs no RDRAND, so a CPU
without it still places images. Diagnostics tell the placement failures
apart: no legal placement, random unavailable, invalid ELF after
relocation, region conflict, and resource exhaustion.

**Exposure.** No new auxv entry carries the base. A program derives it from
`AT_PHDR`, as musl's `_dlstart` and glibc's `_dl_relocate_static_pie`
already do. A `goos_diag` build prints `linux pie base=<hex> span=<hex>`
for each placement; a release build prints nothing.

**Reproducibility.** The build draws no randomness. The ISO and the disk
image stay byte-identical across builds, and `make test-reproducible` does
not change.

## Placement entropy

For a page-aligned image of size `S` whose largest alignment is 4 KiB:

```text
count = floor((1 GiB - 64 MiB - S) / 4 KiB) + 1
```

A small image gets about 2^17.9 bases: an 8 KiB image has 245,759. A larger
alignment divides the count: an image of about 64 MiB whose largest
alignment is 2 MiB gets about 448 bases, near 8.8 bits.

## Non-goals

This decision leaves these layouts fixed:

- `ET_EXEC` images;
- native GoOS images;
- Linux and native stacks;
- anonymous `mmap` placement;
- shared memory;
- brk placement apart from the image it follows;
- kernel image placement;
- fixed GoOS windows.

Each of these needs its own decision. The roadmap lists them as future work.

## Consequences

Two launches of one static PIE run at different bases with high
probability, and an address leaked from one run does not give the base of
the next `execve`. Programs that print their addresses no longer print the same
ones twice.

The fixture `userspace/linux/pie-probe` is a musl static PIE with its own
self-relocating start code. It exercises TLS, RELRO, brk, `getrandom`,
`fork` and `exec`, and reports its base. `make test-linux-aslr` runs
`tests/boot/linux-aslr.sh`: it boots QEMU's `qemu64` CPU, which has no
RDRAND, launches `pie-probe` 32 times from the shell, and requires every
base inside the window and aligned, at least two distinct bases, a fork
child at its parent's base, an `exec` chain that sees more than one base,
a working brk allocation, `AT_RANDOM`, and a successful `getrandom`. It
needs no external binaries and runs in `test-all` and `release-check`.
Host tests in `kernel/compat/linux/elf` cover `PlacePIE`.

An image that needs more than the window, less 64 MiB of brk, no longer
loads; under ADR 0149 the same limit applied to the half above
`0x40000000`.
