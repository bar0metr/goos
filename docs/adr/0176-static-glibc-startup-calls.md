# ADR 0176: The calls a static glibc program makes at startup

## Status

Accepted. Amends the `mprotect` decision of ADR 0104 for Linux processes.

## Context

GoOS ran static musl programs. A static glibc program, 7-Zip's `7zzs` 26.03,
stopped before `main` with "cannot apply additional memory protection after
relocation: Permission denied". A trace of `7zzs i` on Linux 5.15 shows 48
calls; GoOS refused five of them:

- `mprotect` of `PT_GNU_RELRO`, the part of the read-write segment glibc makes
  read-only once it has applied its relocations. The image's segments are
  pinned regions, which `VMProtect` refuses to change (ADR 0104), and the
  refusal reached glibc as `EACCES`. glibc treats it as fatal.
- `set_robust_list`, `readlink("/proc/self/exe")`, `umask`, and a
  `FUTEX_WAKE` nobody waits on. glibc survives each refusal; they still show
  in the refused set and hide the calls that matter.

A 7-Zip round trip (pack, test, unpack, hash) then asked for `gettimeofday`,
`time`, and `sysinfo`, which 7-Zip reads to size its dictionary.

## Decision

**A Linux program may take rights away from its own image.** When `VMProtect`
refuses a range because it is pinned, the Linux `mprotect` path tries
`Map.Restrict`: the range must lie in image regions only, and the new
protection must allow nothing the region does not already allow. Restricting
splits the region like `Protect`, keeping the source offset and file bytes of
each part. A right once removed cannot be given back, and execute can never
be added. Every page of a Linux image is a private copy that `demandFault` or
`installExecutablePage` allocated and filled, never a frame another process
maps, so the narrower protection affects only the caller. Native processes
keep the old rule.

**The startup calls answer as a single-task process on Linux would.**

- `set_robust_list` stores the head and accepts only the 24-byte structure.
  Nothing walks the list: no other task could take over a robust futex.
- `umask` keeps a mask per process, 022 at launch, inherited across `fork`
  and `execve`. FAT32 stores no mode, so the mask changes no file.
- `readlink` and `readlinkat` look the name up as `access` does. GoOS has no
  symbolic links, so an existing name is `EINVAL` and a missing one carries
  the lookup's error. `/proc/self/exe` is `ENOENT`, as on a Linux without
  `/proc`, and glibc falls back.
- `futex`: a wake finds no waiter and returns 0; a wait whose word changed is
  `EAGAIN`; a wait that would sleep, and every other operation, is refused
  and counted. A sleeping wait in a process with one task could never end.
- `gettimeofday`, `time`, and `sysinfo` read the realtime clock, the
  monotonic clock, and the page allocator. `sysinfo` reports no swap, load,
  shared, or buffer memory.

**No change to random bytes.** glibc copies `AT_RANDOM` into its
stack-protector canary and faults on a null pointer when the entry is
missing. ADR 0151 leaves the entry out on a CPU without RDRAND, and that
stands: a glibc program needs a CPU with RDRAND, which Intel CPUs have had
since 2012 and AMD CPUs since 2015. QEMU's default `qemu64` model has none;
a test that runs glibc under QEMU asks for `-cpu max` or `+rdrand`.

A user page fault now logs the faulting instruction (`rip=`) beside the
address. The trace of a program that dies before printing anything starts
there.

## Consequences

`7zzs i`, `a -mmt1`, `t`, `x`, and `h` run on GoOS and produce an archive and
a copy byte-identical to the original. `abi-probe libc` checks every call
above on GoOS and on a Linux kernel.

7-Zip's default two-thread LZMA2 encoder still fails: it creates a thread
with `clone`, and GoOS has no Linux threads, so the clone is `EINVAL` and
7-Zip reports "System ERROR". `-mmt1` avoids it. `utimensat` stays `ENOSYS`
because the VFS cannot set a time, so unpacked files keep the time they were
written; 7-Zip reports success anyway.
