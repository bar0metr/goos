# ADR 0185: A kernel random generator seeded from jitter

## Status

Accepted. Supersedes the random-bytes decision of ADR 0151 and the note on
random bytes in ADR 0176.

## Context

ADR 0151 took random bytes from RDRAND and nowhere else: on a CPU without the
instruction `getrandom` returned `ENOSYS` and the initial stack had no
`AT_RANDOM`. Static glibc reads its stack canary through `AT_RANDOM` before
`main`, so every static glibc program died with a null read on such a CPU.
7-Zip 24.09 (`7zzs`) is one: it ran on a Linux kernel in QEMU's default
`qemu64` CPU and faulted at its first instructions on GoOS in the same VM.
Linux hands out `AT_RANDOM` on every CPU; a program has no other place to
look.

## Decision

The kernel keeps one generator (`kernel/random`): a ChaCha20 stream with a
64-bit block counter, keyed from an entropy pool. After every request the
next 32 bytes of the stream become the key and are never handed out, so
bytes already returned cannot be recomputed from a later state.

The pool takes 64-bit samples and runs through the ChaCha20 permutation every
eight. Boot fills it before the first process exists: up to 32 RDRAND draws
when the CPU has the instruction, then the cycle counter read around a loop
whose length and memory walk depend on the previous reading, at least 4096
times. That jitter comes from cache misses, pipeline state, and interrupts;
it is what Linux seeds from on a CPU without a hardware generator. The
generator refuses to produce anything before the pool has taken 4096
samples. Housekeeping and each `getrandom` stir in more cycle counts, and
every 256 samples rekey the stream.

Every Linux image gets 16 bytes behind `AT_RANDOM`. `getrandom` serves up to
256 bytes per call and never blocks, since boot seeds the pool first.

## Consequences

Static glibc programs start on any x86_64 CPU. Under an emulator the jitter
is less of a secret than on hardware: a host that controls the emulator can
observe it. A key drawn there is as strong as the host is trustworthy, which
was already true of every other secret in the guest. On a CPU with RDRAND the
pool also holds hardware output. The abi-probe's `random` mode now requires
both sources of bytes on every machine, and the general boot test, whose CPU
has no RDRAND, runs it.
