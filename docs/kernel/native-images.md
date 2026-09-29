# Native images

GoOS native executables use a small versioned envelope independent of Go
structure layout. The current format is a bootstrap format, not a declared
stable application ABI.

The 40-byte version 2 header contains the eight-byte `GOOSNAT\0` magic, format
version, header size, segment count, reserved field, entry address, initial
stack pointer, and stack reservation. It is followed by fixed 32-byte segment
records containing virtual address, payload offset and size, in-memory size,
permission flags, and a reserved field. All integers are little-endian.
Version 1 images are refused (ADR 0104).

The loader accepts at most eight segments. Every segment must be page-aligned,
lie in the image area `[0x40002000, 2 GiB)`, have its payload inside the file,
and request known permissions; there is no bound on a segment's size beyond
that. Writable-executable mappings are rejected. Entry must lie inside an
executable segment. Overlapping virtual ranges, truncated headers, invalid
versions, reserved fields, and offset overflow are rejected before anything is
mapped.

An image either leaves its stack to the kernel or carries it. With an initial
stack pointer of zero, the kernel reserves the stack below
`0x00007fffff000000`: the header's reservation, a whole number of pages up to
64 MiB, or 1 MiB when it is zero, with 64 KiB of guard below it. Every TinyGo
program does this; the linker script sets the reservation with
`_goos_stack_reserve`. An image that carries its stack — the kernel's
self-test programs and the fault probe — names a pointer inside a writable,
non-executable segment, and the `StackGuardSize` (64 KiB) below that segment
must be a gap no segment covers and the kernel maps nothing into (ADR 0083,
ADR 0088). `tools/stackframes` checks that no function in a program has a stack
frame deep enough to step over the guard.

`elf2native` stores only a segment's bytes up to its last non-zero byte, so
BSS is not part of the file.

## Loading

Each segment becomes a pinned image region of the process (ADR 0104) that
records where its bytes come from: an extent of the system image or an
executable source (ADR 0106, ADR 0121). No private page is loaded at launch. When the
process first touches a page, the kernel copies it from that origin, or zeroes
it past the segment's file bytes, before mapping it with the segment's final
permissions, which preserves W^X. A large program therefore starts in the
time a small one does and costs only the pages it uses.

Read-only segments of images from the system image are loaded once and their
frames shared by every process running the image (ADR 0081). An executable
source stays alive while any process started from it runs. A VFS-backed source
retains only its header page; other pages are read from its stable open object
on demand.

Relocations and executable ASLR remain future work. Images are static
`ET_EXEC` programs linked at `0x40002000`; what randomizing their placement
would change in this format is described in
[memory.md](../architecture/memory.md#aslr-readiness). Linux ELF is a separate
compatibility-layer format and does not define this native envelope.
Successful validation selects the immutable `Native` Process ABI. Linux ELF
launch is separate and selects `LinuxX8664`; neither path changes
personality after process initialization (ADR 0132, ADR 0133). The Linux parser
validates a bounded static ELF64 load plan through the same stable
executable-source abstraction. Its loads use the same lazy image region model,
including file-backed faults, demand-zero BSS, accounting, and W^X. Linux gets
a separate System V initial stack without exposing `GST4`; the common launch
path applies the constrained Linux authority policy.

Process startup is a separate versioned ABI described by ADR 0031. The current
`GST4` object (ADR 0109) is limited to 64 KiB and carries the command, the working
directory, `argv` and environment bounded by bytes rather than counts, and named
process-local handle bindings. Keeping startup
metadata outside the image format allows one immutable image to run under
different capability manifests.
