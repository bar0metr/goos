# Physical memory

Early boot allocates a fixed 256 KiB UEFI memory-map buffer, captures the final
map, normalizes firmware types into architecture-neutral regions, and calls
`ExitBootServices` with the matching map key. A stale key is retried without
allocating between map capture and exit. After success, firmware boot services
must never be called again.

The first physical allocator uses a fixed bitmap and manages physical addresses
below 1 GiB. This covers the supported 512 MiB QEMU configuration without
requiring dynamic metadata during allocator bootstrap. Page zero through the
first MiB stays reserved. Only UEFI conventional memory is initially free;
loader, runtime, MMIO, ACPI, and firmware regions stay reserved. Boot-services
memory is tagged reclaimable but remains reserved until later code proves no
firmware-owned structures reference it.

Allocation and release operate on 4 KiB pages. Unaligned, unmanaged, low-memory,
and double-free requests are rejected. The boot path allocates and releases one
page as a live self-test before emitting `PMM_OK`. The search starts after the
last allocation, falls back to the lowest freed page, skips bitmap words that
have no free page, and finds the first free bit with a trailing-zero count, so a
long used run costs one test per 64 pages (ADR 0057). Allocation, release, and
every other change to allocator state happen inside the kernel exclusion domain
of ADR 0053.

## Virtual-memory bootstrap

The kernel replaces the firmware page tables after `ExitBootServices`. The
bootstrap map uses four-level x86_64 tables and 4 KiB leaves for the first GiB.
The first MiB remains unmapped to catch null and low-address accesses. Every
mapping is supervisor-only.

Ordinary identity-mapped pages are writable and non-executable. The loaded PE
image is validated in memory and its section flags are translated into leaf
permissions. Executable sections are read-only; writable sections are NX. A PE
section requesting write and execute simultaneously is rejected. EFER.NXE and
CR0.WP are enabled before the kernel installs its PML4 through CR3.

Process roots clone the supervisor-only kernel PDPT path and allocate private
user page-table levels. User leaves require the user bit at every parent, reject
W+X, and are zeroed before exposure. The bootstrap native program uses a
separate RX code page, read-only data page, and RW+NX stack page.

Kernel APIs do not directly dereference user virtual addresses. `CopyIn` and
`CopyOut` reject arithmetic wraparound and addresses outside the bootstrap user
range, then validate every page crossed by the request. Every page-table level
and leaf must be present and user-accessible; `CopyOut` additionally requires a
writable leaf. The validated physical pages are accessed through the kernel
identity map with quadword string instructions, which also clear page-table
pages and initialize loaded image pages.

## Shared image pages

The read-only segments of images from the system image — code and constants —
are loaded once and mapped by every process that runs the image (ADR 0081).
The frames are keyed by the image's offset in the immutable archive, mapped with
the segment's own permissions, which never include write, and mapped without the
owned bit, so a process's teardown leaves them. They are not charged to any
process and are never freed. Writable segments and stacks remain private copies.
The cache is bounded at 128 segments and 1024 frames; a launch that does not fit
loads a private copy.

## Shared memory

Shared-memory objects own between one and 16 zeroed physical pages. A monotonic
registry assigns their nonzero identities in a separate object namespace.
Processes receive only local memory
handles; mappings require Read authority and writable mappings additionally
require Write. Transfer derives equal or narrower rights in the receiver.

The mapper validates the complete object page range and all destination leaves
before installing mappings. The same physical pages may appear in different
process roots or at different virtual addresses. Every shared mapping is NX.
Executable shared memory requires a future explicit write-then-seal design.

The boot security test maps a two-page object writable in process A and read-
only in process B. A payload crossing the page boundary is written through A,
read through B, and verified to resolve to identical physical pages. A write
through B is rejected.

The one-GiB kernel identity map is a bootstrap constraint, not the final process
layout. A higher-half kernel, complete guard regions, demand paging, and ASLR
remain later work.
