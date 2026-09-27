# ADR 0103: Physical memory beyond the first GiB

## Status

Accepted. Replaces the one-GiB limit of ADR 0002 and extends ADR 0003. ADR
0120 supersedes the process-root inheritance and low kernel-stack parts of
this decision; the allocator and direct-map design remain current.

## Context

The page allocator of ADR 0002 was a fixed bitmap for physical addresses below
1 GiB, and usable RAM above it was ignored. Every page the kernel touched was
reached at its physical address through the supervisor-only identity map of
the first GiB (ADR 0003). That map cannot simply grow: the user range is
`[1 GiB, 2 GiB)` in the same first PML4 slot, and moving it is a separate
change.

The 0.1.3 specification (§10) asks for the allocator to manage every usable
range, including RAM above 4 GiB, which q35 places there once the guest has
more than 2.75 GiB; to keep reserving everything that is not free; to keep the
accounting and the zeroing guarantees (ADR 0048); and to do it with bounded
metadata and a simple structure, not a buddy allocator.

Booting with more memory showed a second problem. The kernel did not start at
all above about a GiB. OVMF allocates loader memory from the top of low RAM,
and the loader memory includes the 85 MiB arena TinyGo's runtime allocates
before any kernel code runs, which holds the kernel heap and the stack `main`
runs on. With 512 MiB it lies at `0x16617000`; with 2 GiB or 4 GiB at
`0x76617000`, inside the user GiB and outside the identity map, and the kernel
stopped with `unsafe virtual-memory bootstrap state`. The ACPI tables moved
above 1 GiB too, past the bound the boot-time table reader enforced.

## Decision

**Segments.** `pmm.Allocator` is a bounded array of `MaxSegments` (64)
segments in ascending address order. A segment is one contiguous run of usable
pages at or above 1 MiB with its own slice of bitmap, one bit per page. Only
UEFI conventional memory is usable, as before. Segments are built in address
order whatever order the firmware lists its descriptors in; adjacent or
overlapping usable descriptors merge, and no segment straddles 1 GiB. Every
other kind of memory — the first MiB, the kernel image and loader memory,
boot-services and runtime memory, ACPI, MMIO — lies outside every segment, so
a free of it is refused and it is never reported as allocated. Usable memory in
ranges beyond the 64th is left unmanaged and counted.

**Cap.** The allocator manages physical memory below `pmm.MaxPhysical`,
512 GiB, the span of the one PML4 slot that holds the direct map. Usable memory
above it is left unmanaged and counted.

**Metadata.** The bitmaps are carved at `Init` from the top of the highest low
segment that can hold them, so low allocation still starts at 1 MiB. The pages
stay marked used: they count as managed and used, and `Free` refuses them. The
package has no `unsafe`, so the storage is reached through a `pmm.MetadataView`
the caller supplies: `vm.PhysicalWords` returns the words at their physical
address. `Init` runs while the firmware's page tables still identity-map all
memory, and `BuildIdentity` maps the same low addresses afterwards. The cost is
one bit per managed page — 32 KiB per GiB, 16 MiB at the cap — plus 4 KiB for
the allocator itself, which was 32 KiB before. If low memory cannot hold the
bitmap, the highest segments are dropped, and counted as unmanaged, until it
can.

**Two allocation classes.** `Allocate` returns a page from the highest segment
that has one, so memory below 4 GiB, which 32-bit devices can reach, and memory
below 1 GiB, which is also a kernel pointer, go last. `AllocateLow` returns a
page below 1 GiB, scanning the low segments upward from the last allocation as
before. `AllocateContiguous` stays low-only, and a run never crosses a segment
boundary, so it never spans a reserved hole. Every caller was assigned a class:

| Caller | Class | Why |
| --- | --- | --- |
| user image pages (`loader`), startup page (`launch.go`), shared-memory objects (`shmem`), executable objects (`executablecalls.go`), self-test process pages, the boot allocate-and-free check | `Allocate` | reached only through `InitializePhysical`, `PhysicalPage`, `CopyIn`, and `CopyOut` |
| page tables (`vm.allocateTable`) | `AllocateLow` | every walker uses a table's physical address as a pointer |
| boot, task, and IST stacks with guard pages (`arch/amd64`) | `AllocateContiguous` | used at identity addresses; guards are unmapped in the identity map |
| DMA window (`devicecalls.go`) | `AllocateContiguous` | a device-visible physical address; kept below 4 GiB for legacy virtio |

**Direct map.** `BuildIdentity` maps every segment at
`DirectMapBase + physical`, with `DirectMapBase = 0xffff800000000000`, the
start of PML4 slot 256. Leaves are two MiB, supervisor-only, writable and NX;
one-GiB pages are not used because QEMU's default CPU model lacks them. The
tables are low pages: a PDPT and one directory per GiB touched, three pages at
4 GiB and 513 at the cap. Every process root copies slot 256 from the kernel
root. The entry has no user bit, so user code cannot reach it and
`DestroyUserAddressSpace`, which only follows user entries, never frees it.
The two-MiB leaves may reach a little firmware memory past a segment's ends;
the kernel only uses the recorded segment ranges.

**One accessor.** Every kernel access to an arbitrary physical page goes
through `kernelAddress`: `CopyIn`, `CopyOut`, `CanCopyOut`,
`InitializePhysical`, `InitializePhysicalByte`, and `PhysicalPage`. It returns
the physical address itself for `[1 MiB, 1 GiB)` and `DirectMapBase + physical`
above that, but only inside a managed segment; it refuses the first MiB,
unmanaged high memory, and ranges that wrap or straddle 1 GiB. Low memory keeps
its identity address rather than always using the direct map because it holds
memory the kernel must reach that is not managed: the boot archive, which
`ImageMap` maps into processes, is loader memory, and a native call may pass a
pointer into it. It also leaves the 512 MiB system exactly as it was. Kernel
stack guards are unaffected: the direct map aliases the guard pages, but stacks
are only used at their identity addresses.

**Loader memory above 1 GiB.** A new kind, `boot.MemoryLoader`, names UEFI
loader code and data. `vm.MapLoaderMemory` maps loader memory above 1 GiB at
its physical address, supervisor-only, writable and NX, in the kernel's first
PML4 slot. The part inside the user GiB sits in a kernel directory, whose
entries `CreateUserAddressSpace` copies into each process's user directory.
The entries have no user bit, so user access faults and `CopyIn` and `CopyOut`
refuse them. `MapUserPage` and `CanMapUserPage` now require the user bit at
every parent level, so no user leaf is ever written into a table the kernel
shares; `ExtendUserAddressSpace` refuses a kernel entry; teardown skips them.
Loader memory inside `[1 GiB, UserImageLimit)`, where the fixed user windows
are, is refused at boot. On q35 the arena and the memory-map buffer occupy
directory entries 435 to 492 of the user GiB, and the windows use entries 0
to 2. This is a bridge: once the user range leaves the low half, loader memory
is identity-mapped like the rest of the kernel.

**Firmware tables.** The boot-time ACPI reader is bounded at 4 GiB instead of
1 GiB. It runs before the kernel's page tables are loaded, under the
firmware's identity map, and firmware keeps its tables below 4 GiB.

**Accounting.** `Stats.ManagedPages` counts the usable pages the allocator
owns, including its bitmap. Before, it was the highest managed page number, so
`UsedPages` included firmware holes below it. `FreePages` and `UsedPages`
keep their meaning; `HighPages` counts managed pages above 1 GiB and
`UnmanagedPages` usable pages left out. `mem` and the `SystemInfo` call report
managed memory without a wire change. The boot banner prints
`Memory: N MiB managed`, and a second line when memory is left unmanaged.
Pages are still zeroed or filled through `InitializePhysical` before any
process sees them; for high pages this goes through the direct map.

**Verification.** `MEMORY` (default `512M`) sets the guest RAM for `make run`
and every QEMU test. The validation kernel allocates a high page when there is
one, fills and reads it through the direct map, maps it into a new address
space, reads and writes it with `CopyIn` and `CopyOut`, frees it, checks the
free count, and prints `PMM_HIGH_OK` with the page's address, or
`PMM_HIGH_NONE`. The boot test requires `PMM_HIGH_NONE` at 1 GiB or less,
`PMM_HIGH_OK` from 2 GiB, and a page above 4 GiB from 3 GiB.

## Alternatives

A buddy allocator is excluded by the specification and brings nothing here:
the only contiguous requests are a few low pages for stacks and DMA.

One flat bitmap up to the highest usable address is simpler but pays for every
hole: the 2 GiB PCI hole at 4 GiB, and on a machine with memory near the cap,
16 MiB whatever its size. A two-level sparse bitmap with a fixed directory of
chunks would also work; segments follow the firmware map directly and give the
split between the classes for free. A static bitmap for the cap would put
16 MiB in the kernel's BSS.

Extending the identity map above 1 GiB collides with the user GiB. One-GiB
pages in the direct map would save a few page-table pages and fail on CPUs
without `pdpe1gb`. Routing low memory through the direct map as well would
leave the boot archive, which no segment covers, unreachable from `CopyIn`.

The loader memory could have been kept low instead. TinyGo's runtime
allocates its arena with `AllocateAnyPages` before any kernel code runs, so the
kernel cannot choose the address; QEMU's `max-ram-below-4g=1G` would hide the
problem on QEMU only, and leave RAM between 1 GiB and 4 GiB unexercised.

## Consequences

All usable RAM up to 512 GiB is managed. On q35 with OVMF, the kernel manages
366 MiB at 512 MiB, 1902 MiB at 2 GiB, and 3950 MiB at 4 GiB, and the
validation boot uses a page at `0x7fe22000` and at `0x100022000` respectively.
Processes get their memory from the top of RAM first, and the first GiB is
left to kernel structures.

A page from `Allocate` may not be used as a pointer: kernel code reaches it
through `PhysicalPage` or `InitializePhysical`. Anything that stores or
dereferences a physical address must use `AllocateLow` or
`AllocateContiguous`. Low memory can run out while high memory is free, in
which case page tables and stacks fail; the first GiB is large next to the
kernel's structures.

On machines with more than about a GiB, every address space has
supervisor-only kernel directory entries in its user GiB until the user range
moves. The limits are 64 segments, 512 GiB, and the 128 firmware descriptors
`boot.Info` holds, which is unchanged.
