# ADR 0083: User stack guard pages

## Status

Accepted. Extends ADR 0011 and ADR 0081. ADR 0088 widens the guard from one
page to a 64 KiB gap, bounds every frame by it at build time, and replaces the
fault probe's recursion with one through a frame larger than a page.

## Context

Kernel-entry stacks have had an unmapped guard page below them from the start,
but user stacks did not. The userspace linker script placed the 32 KiB stack
directly after `.data` and `.bss` in the same writable segment, so a process
that overflowed its stack silently overwrote its own globals and heap and went
on running with corrupted state. It could not reach another process or the
kernel, but the failure was undetected and its symptoms appeared far from the
cause. The specification requires guard pages for stacks.

The native loader cannot tell how a stack is laid out from the image's point of
view; it only knows the segments and the initial stack pointer. A guard that
depends on one linker script protects only the images that script builds.

## Decision

**The stack is its own segment with an unmapped page below it.** The userspace
linker script emits the stack as a fourth `PT_LOAD` segment, read-write and
non-executable, one page above the end of the data and heap segment. The page
in between belongs to no segment and is never mapped, so the first push past
the bottom of the stack takes a not-present page fault. The heap keeps its
place after `.bss`; an access past its end also meets the gap.

**The loader enforces the guard for every image.** `ParseNative` finds the
segment holding the initial stack pointer and rejects the image unless the page
directly below that segment lies inside the user range and no segment covers
any part of it. `LoadImage` additionally rejects a guard page that the kernel
maps itself once the image is loaded: the probe and startup pages below
`0x40002000` and the shared-memory, DMA and system image windows. `elf2native`
runs the parser on its output, so a linker change that loses the gap fails the
build instead of the boot.

**An overflow is an ordinary user fault.** The fault path already retires the
faulting task with the vector as its exit code and lets the normal teardown
release the process; the kernel does not panic. That path now also records the
faulting address of a user page fault for its task slot, so the kernel can say
which page was touched.

**The validation build proves it at run time.** The fault-probe image that PID 1
supervises now recurses with `call` until its one-page stack runs out, instead
of executing `ud2`. For each exit the kernel checks that the task died of a
page fault inside the page below the probe's stack and that the page is unmapped
in its address space, and it requires at least one such exit before printing
`USER_STACK_GUARD_OK`. The supervisor keeps relaunching the probe and then
starts the system services, so the same boot shows that other processes keep
running. The fixed self-test layout moves its stack to `0x40006000`, leaving
`0x40005000` unmapped above the data page.

W^X, page alignment and the shared read-only page cache are unchanged: the
stack segment is writable and therefore always private.

## Alternatives

**Guard only through the linker script.** It fixes TinyGo programs but leaves
any other image, including hand-built ones such as the self tests, unguarded.
Checking in the loader costs one pass over at most eight segments.

**A guard the loader inserts itself.** The loader could reserve the page below
the stack segment regardless of the image. But an image that places another
segment there would then fail to load for a reason its format does not state;
making the gap part of what a valid image is keeps the rule visible and
testable on the host.

**Stack at the top of the user range, growing towards the image.** It matches
conventional layouts and leaves a large hole below the stack. It would make the
kernel, not the image, choose the stack's place, which the native format has so
far left to the image, and it guards nothing a single unmapped page does not
while stacks are fixed at 32 KiB.

**Larger guards or stack probes.** A frame larger than a page could step over a
single guard page. The userspace target does not enable stack probes, and the
userspace programs keep frames small, so one page catches runaway recursion,
the usual cause. A larger gap costs only address space and can be added if needed.

## Consequences

A user stack overflow now terminates the process with a page-fault exit code
that its supervisor sees, instead of corrupting data. Each TinyGo image carries
one more segment record (32 bytes) and nothing else; the gap costs no memory.

Images whose stack segment sits directly above another segment, as every image
built before this change did, are rejected by the loader. All images are
rebuilt from source with the system, so none are affected.

The per-slot fault address is recorded for every user page fault, in normal
builds too, but only the validation build reads it. It is a natural source for
richer fault reports later.
