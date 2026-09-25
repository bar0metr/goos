# ADR 0081: Shared read-only image pages

## Status

Accepted. Extends ADR 0011 and ADR 0080. Since ADR 0093 the cache key is the
launch extent (`offset<<32 | length`) rather than the member's archive offset.

## Context

The loader copied every segment of an image into fresh frames for every process
that ran it. With commands as separate images (ADR 0080) each launch copied its
code and constants again, and under QEMU's TCG, where GoOS runs, a launch paid
twice: once for the copy and again for translating code in pages the emulator
had never seen. ADR 0079 measured that the translation, not instruction count,
dominates a utility's startup.

Sharing the code of one program between its processes is what every operating
system with executables does. The question is whether it can be done without
weakening what GoOS relies on: W^X, process isolation, the immutability of the
system image, and per-process resource accounting.

## Decision

**Read-only segments of system images are backed by shared frames.** The first
launch of an image copies each segment without the writable flag into frames the
kernel keeps; every later launch maps those frames. Writable segments — data and
stack — stay private copies. The parser already requires segments to be page
aligned and never both writable and executable, so a shared and a private
segment never meet in one page.

**The frames are never writable in any process.** They are mapped with the
segment's own permissions, which exclude writing. The kernel writes into user
memory only through `CopyOut`, which requires a writable leaf, and no operation
changes a mapping's permissions. A process can therefore neither write its code
itself nor have the kernel do it.

**The key is the image's place in the immutable archive.** `initramfs.Locate`
names an image by the offset of its bytes in the system image, the same for
every path and hard link that leads to them and fixed for the boot. Nothing a
process supplies selects frames. Images that do not come from the archive — the
boot-time self-test programs and PID 1 — load privately as before.

**The frames belong to the system image, not to a process.** They are mapped
without the owned bit, so a process's teardown leaves them, and they are not
charged to the launching process's page quota: the launch that fills the cache
subtracts what it added. They are never freed, which matches the lifetime of
the image they hold.

**The cache is bounded and optional.** It holds at most 128 segments and 1024
frames. A launch that does not fit, or whose allocation fails, loads a private
copy; sharing is an optimisation that cannot make a launch fail.

## Alternatives

**Mapping the archive's own pages.** The initramfs is already in memory, and
the filesystem service maps it read-only (ADR 0070). But a tar member's bytes
start on a 512-byte boundary, not a page boundary, so a segment's pages do not
coincide with archive pages. Aligning members in the archive would allow it and
save the copy entirely; it can come later without changing the rule that shared
pages are read-only.

**Reference-counted frames freed when the last process exits.** It returns
memory an unused command holds. It also adds a counter the teardown path must
keep exact, for a cache that is at most a few hundred kilobytes over an image
that never changes.

**Demand paging of code.** Mapping pages on first touch would avoid copying code
a process never runs. It needs a page-fault handler that loads rather than
kills, which is a larger change than this and orthogonal to sharing.

## Consequences

At 100 Hz under TCG, measured against the previous build on the same host:
`uname` takes 1.5 ms instead of 3.8 ms, `cat /system/motd` 3.3 ms instead of
11.1 ms, a three-stage pipeline 3.0 ms instead of 10.2 ms, a persistent write
10.9 ms instead of 20.5 ms, and `ls /system` 6.5 ms instead of 17.8 ms. Commands
that launch nothing are unchanged. On hardware or under KVM the translation
cost does not exist, and what remains is not copying code and holding one copy
of it.

The validation build checks both halves. The kernel loads one image into two
address spaces through a private cache and requires one code frame mapped by
both, private stacks, refusal of a kernel write into the shared frame, and that
tearing one process down leaves the frame allocated and mapped in the other
(`IMAGE_PAGES_ISOLATED_OK`). The supervision scenario, which relaunches one
image, must have mapped from the launch cache (`IMAGE_PAGES_SHARED_OK`).

Processes running the same image now share physical pages, which allows cache
timing attacks such as Flush+Reload between them. Side channels are out of scope
for 0.1 (threat model); a build that must exclude them can pass no cache to the
loader.

`ps` reports smaller per-process memory, because a process is no longer charged
for code it shares. The cache appears as used memory in `mem`.
