# Physical memory

Early boot allocates a fixed 256 KiB UEFI memory-map buffer, captures the final
map, normalizes firmware types into architecture-neutral regions, and calls
`ExitBootServices` with the matching map key. A stale key is retried without
allocating between map capture and exit. After success, firmware boot services
must never be called again.

The physical allocator manages every usable range below 512 GiB as a bounded
array of at most 64 segments, each a contiguous run of usable pages with its
own bitmap (ADR 0103). Page zero through the first MiB stays reserved. Only
UEFI conventional memory is free; loader, runtime, MMIO, ACPI, and firmware
regions stay outside every segment. Boot-services memory is tagged reclaimable
but remains reserved until later code proves no firmware-owned structures
reference it. Segments are built in address order, whatever order the firmware
lists them in, and none straddles 1 GiB. Usable memory above 512 GiB or in
ranges beyond the 64th is left unmanaged; the boot banner reports it.

The bitmaps, one bit per managed page, are carved at boot from the top of low
memory and stay marked used. The allocator reaches them through the firmware's
identity map while it initializes and through the kernel's afterwards.

Allocation and release operate on 4 KiB pages. There are two classes.
`Allocate` takes from the highest segment that has a free page, so process
memory comes from the top of RAM; such a page may lie above 1 GiB and is
reached only through the direct map. `AllocateLow` and `AllocateContiguous`
return pages below 1 GiB, whose physical address is also a kernel pointer:
page tables, kernel stacks with their guard pages, and the DMA window use
them. A contiguous run never crosses a segment boundary. Unaligned, unmanaged,
reserved, bitmap, and double-free requests are rejected. The boot path
allocates and releases one page as a live self-test before emitting `PMM_OK`;
the validation build also exercises a page above 1 GiB, when there is one,
through the direct map and a process mapping (`PMM_HIGH_OK`). Within a
segment, the search starts after the last allocation, falls back to the
lowest freed page, skips bitmap words that have no free page, and finds the
first free bit with a trailing-zero count, so a long used run costs one test
per 64 pages (ADR 0057). Allocation, release, and every other operation take
the allocator's own lock, so processors allocate and free concurrently and no
page reaches two owners (ADR 0182). Unmapped process pages return to it only
after a TLB shootdown (ADR 0183).

Each process's address space has a reader-writer lock over its region map
and page tables (ADR 0184). Copies to and from user memory and demand faults
hold it for reading, so the threads of a process fault on several processors
at once: a page table or a leaf goes in with a compare-and-swap, and the
fault that loses the race finds the page present and runs its access again.
`mmap`, `munmap`, `mprotect`, `madvise`, and the native mapping calls hold it
for writing, and so does the shootdown that follows a change. A page an
executable object holds is brought in with the call's other locks released
and mapped by the faulting task.

The allocator counts the usable pages it manages, its bitmap included, and
how many are free; `mem` reports both. `make MEMORY=4G run` and
`make MEMORY=4G test-boot` boot a guest with more RAM; the default is 512 MiB.

## Virtual-memory bootstrap

The kernel replaces the firmware page tables after `ExitBootServices`. The
bootstrap map uses four-level x86_64 tables and 4 KiB leaves for the first GiB.
The first MiB remains unmapped to catch null and low-address accesses. Every
mapping is supervisor-only.

The direct map places every managed physical page at
`vm.DirectMapBase + physical` (`0xffff800000000000`, PML4 slot 256) with
two-MiB RW+NX leaves. Every process root shares the slot, without the user
bit, and teardown never frees it. The kernel reaches an arbitrary physical
page through one accessor: pages in `[1 MiB, 1 GiB)` at their physical
address, managed pages above that through the direct map. On a machine with
more than about a GiB, the firmware places the loader memory that holds the
kernel heap above 1 GiB; `vm.MapLoaderMemory` maps it at its physical address
in the kernel root. Process roots do not inherit those low mappings (ADR 0103,
ADR 0104, ADR 0120).

The GOP framebuffer uses a separate 512 MiB kernel window beginning at
`0xffff808000000000` (PML4 slot 257), immediately after the direct-map slot.
Only the page-aligned physical range required by stride times height is mapped.
Its leaves are supervisor-only, writable, NX, and uncached through PCD+PWT.
Before the mapping becomes active, the kernel programs `IA32_PAT` to the
architectural `WB, WT, UC-, UC, WB, WT, UC-, UC` layout while caching is
disabled and flushes caches and TLBs. PCD+PWT therefore selects a known UC
entry rather than depending on firmware PAT state.
The slot exists only in the kernel root; process roots do not copy it. Before
the PMM is initialized, boot rejects graphics when any mapped framebuffer page
overlaps a UEFI conventional-memory region, so device pages can never enter
the allocator's free pool (ADR 0123).
The framebuffer terminal keeps only cursor, UTF-8, and bounded escape-parser
state. Glyph rendering writes the mapped pixels directly, and scrolling uses
overlap-safe in-place copies; it allocates no shadow framebuffer or history.

Ordinary identity-mapped pages are writable and non-executable. The loaded PE
image is validated in memory and its section flags are translated into leaf
permissions. Executable sections are read-only; writable sections are NX. A PE
section requesting write and execute simultaneously is rejected. EFER.NXE and
CR0.WP are enabled before the kernel installs its PML4 through CR3.

Process roots share the direct map but not the low kernel image. They privately
map only the high assembly entry and return pages, descriptor metadata, and
the current task's high kernel stack. Those mappings remain supervisor-only.
Entry code changes to the kernel CR3 before it transfers to low-linked TinyGo
code; the high return prefix changes back to the process CR3 immediately
before `iretq` (ADR 0120). Every user page-table level is private and created
on demand. User leaves require the user bit at every parent, reject W+X, and
are zeroed before exposure.

Kernel APIs do not directly dereference user virtual addresses. `CopyIn` and
`CopyOut` reject arithmetic wraparound and addresses outside the user range,
then validate every page crossed by the request. Every page-table level and
leaf must be present and user-accessible; `CopyOut` additionally requires a
writable leaf. An absent page is first offered to the demand-fault resolver,
exactly as a touch by the process would be, so a call may name memory the
process has not touched yet. The validated physical pages are accessed through
the kernel identity map or the direct map with quadword string instructions.
User leaves are placed only under page tables that carry the user bit at every
level, so a kernel table shared between address spaces never receives one.

## User address space and regions

User addresses are canonical lower-half addresses in `[0x1000,
0x00007ffffffff000)` (ADR 0104):

| Range | Use |
| --- | --- |
| `0x00400000` | self-test probe page (validation build) |
| `0x00402000` | low-address self-test code (validation build) |
| `0x00404000` | low-address self-test data (validation build) |
| `0x1000` – `2 GiB` | image segments; native images normally start at `0x40002000`, Linux `ET_EXEC` may use conventional low addresses such as `0x400000`, a Linux static PIE loads at `0x40000000` |
| `0x0000080000000000` | shared-memory window for callers that pass a fixed address (file cache, exec area) |
| `0x0000081000000000` | DMA window |
| `0x0000082000000000` | system image window |
| `0x0000083000000000` | PID 1 launch staging page |
| `0x0000084000000000` | process startup object, read-only |
| `0x0000100000000000` – `0x0000700000000000` | addresses the kernel chooses: heap, anonymous maps, shared maps |
| below `0x00007fffff000000` | stack reservation, guard below it |

Every process has a region map (`kernel/mm/region`): a sorted table of at
most 128 non-overlapping regions, each with a protection, a backing
(anonymous, image, shared, device, guard), and flags. The page tables say which
pages are present; a region says what a page may become. The kernel's own
structure — image segments, the stack reservation and its guard, the startup
page, the DMA and image windows, PID 1's staging page — is recorded as pinned
regions the native calls cannot unmap or reprotect.

**Demand-zero memory.** Anonymous memory and the zero-filled tail of an image
segment are supplied on first touch. The user page-fault path saves the
registers and SSE state and asks the kernel to resolve the fault: an absent
page inside a demand-zero region whose protection allows the access gets a
zeroed page, charged to the process with any page tables it needed, and the
instruction is retried. Anything else — a guard region, an access the
protection refuses, an address in no region, a spent page quota — retires the
process and logs which it was (`user fault: guard page`, `access not
permitted`, `no mapping`, `page quota exhausted`). A fault in kernel mode is
still fatal.

Linux ELF mappings use the same mechanism. A validated `PT_LOAD` becomes a
pinned image region whose source offset and byte count cover the ELF-aligned
file window. First touch reads bytes through `p_filesz`; the remainder of the
page-rounded `p_memsz` range is zero-filled. The Linux stack is an 8 MiB
anonymous RW/NX reservation ending at `UserStackTop`, with a 64 KiB guard, and
also grows through demand-zero faults. Its initial System V image is capped at
64 KiB; only pages holding `argc`, vectors, auxv, and strings are populated at
launch, while the rest remains absent.

**Native calls.** `VMMap` reserves anonymous memory at a free address the
caller names or one the kernel chooses; `VMUnmap` removes a range and frees the
owned pages in it; `VMProtect` changes the protection of anonymous memory (to
none, read-only, or read-write — never executable); `VMDiscard` frees the pages
of anonymous memory and keeps the reservation; `VMQuery` describes the region
at an address. `MemoryMap` of a shared-memory object records a shared region
and may let the kernel choose the address. Every range is checked for
alignment, wraparound, and the user range, and W^X holds for every mapping.
Page tables, once created, stay with the address space until it is destroyed.

**Stack and heap.** An image no longer carries its stack or heap. The kernel
reserves the stack below `0x00007fffff000000` — 1 MiB by default, as much as
the image header asks up to 64 MiB — above 64 KiB of guard, the gap
`tools/stackframes` bounds every frame by (ADR 0088), and supplies the top page
at launch and the rest as the stack grows. The userspace runtime reserves a
256 MiB heap at start; only the pages it touches count. Self-test and
fault-probe images may still carry their own stack segment, with the same
64 KiB gap below it.

**Userspace heap.** Native programs reclaim ordinary Go objects (ADR 0201,
ADR 0202). Allocations up to 2 KiB share slab pages of one of fourteen size
classes, from 16 to 2048 bytes with 1360 between 1024 and 2048; larger ones
take runs of whole pages. Page metadata lives in its own reservation, outside the
pages it describes. A per-process, stop-the-world, non-moving, conservative
mark and sweep, which keeps objects the compiler marks pointer-free without
reading them, finds roots in the globals the linker script brackets, the
stack from the collector's frame up to the stack top `_start` records, the
callee-saved registers, and the used part of every region. It runs on
`runtime.GC`, when live bytes reach `max(2 × live, live + 1 MiB)` of the last
collection, when an allocation does not fit, and when a service has waited
100 ms with nothing to do; `arena.OnExhausted` runs only after a collection
and a retry fail. The collector first zeroes up to 4 KiB of stack below itself,
never past the bottom of the stack, so its own frames hold no stale address. Free pages go back with `VMDiscard`
after each collection. `arena.SetRetained` can keep up to 256 of them for the
allocations that follow, and an idle wait or a failed allocation gives those
back too; by default none is kept, which a guest workload measured fastest.
`arena.ReadDiagnostics` reports what the allocator counted, and
`make allocator-report` puts host and guest measurements side by side.
`arena.Region` serves scratch with one lifetime from its own reservation, and
no collection runs while one is entered. The kernel keeps the separate
discipline of ADR 0065: a leaking boot-time heap and no allocation on repeating
paths.

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

Shared-memory objects own between one page and 256 MiB of zeroed physical
pages, kept in a page list. The kernel creates the file cache and the exec and
staging pages at boot; a process creates its own with `MemoryCreate`, charged
to its page quota (ADR 0107). Every handle and every mapping of an object is a
reference, and the last one gone frees it. A monotonic counter assigns their
nonzero identities in a separate object namespace.
Processes receive only local memory
handles; mappings require Read authority and writable mappings additionally
require Write. Transfer derives equal or narrower rights in the receiver.

The mapper validates the complete object page range and the destination region
before installing mappings, and records the mapping as a shared region. The same physical pages may appear in different
process roots or at different virtual addresses. Every shared mapping is NX.
Executable shared memory requires a future explicit write-then-seal design.

The boot security test maps a two-page object writable in process A and read-
only in process B. A payload crossing the page boundary is written through A,
read through B, and verified to resolve to identical physical pages. A write
through B is rejected.

The kernel root retains the low linked image and its higher-half alias.
Process roots omit the low kernel image and map only the high transition state
described in ADR 0120. A relocatable higher-half kernel and ASLR remain later
work.

## ASLR readiness

GoOS has no address-space layout randomization of any kind. Every address
below is the same on every boot and in every process. The specification
(§11.4) allows full ASLR to come after the first milestone, provided the API
and the memory layout do not prevent it. This section records what is fixed,
what each kind of randomization would have to change, and whether anything
decided so far is in the way.

### What is fixed today

| Region | Address | Fixed by |
| --- | --- | --- |
| Kernel PE image | `0x400000` | TinyGo's `uefi-amd64` target links with `--image-base 0x400000 --no-dynamicbase`; `vm.KernelImageBase` reads the PE header there |
| Kernel identity map | physical = virtual for `[1 MiB, 1 GiB)` | `vm.BuildIdentity`; linked kernel code, the heap arena, page tables, stack backing, and the boot archive remain addressable at their physical address |
| Loader memory above 1 GiB | physical = virtual, supervisor-only | `vm.MapLoaderMemory`; where OVMF puts the heap arena on machines with more than about a GiB |
| Direct map | `0xffff800000000000` + physical | `vm.DirectMapBase`; every managed page, shared by all address spaces |
| GOP framebuffer | `0xffff808000000000` + page offset | Dedicated 512 MiB kernel-only RW+NX+UC window; mapped visible stride range only |
| Process entry mapping | minimal supervisor-only pages at `0xffffc00000000000` + linked address | ADR 0120; assembly trampolines, descriptor state, and the active kernel stack |
| User pointer range | `[0x1000, 0x00007ffffffff000)` | `vm.ValidateUserRange` (`UserAddressStart`..`UserAddressLimit`) |
| User page tables | created on demand at every level | `vm.MapUserPageAt` |
| Startup object | `0x0000084000000000` | `vm.UserStartupAddress`; read only through `StartupRead` |
| Image base (code, then rodata, data, bss) | `0x40002000` | `userspace/runtime/amd64.ld`; images are static `ET_EXEC` files below 2 GiB |
| Initial stack pointer | `0x00007fffff000000` | `vm.UserStackTop`; the reservation below it comes from the image header |
| Userspace heap | chosen by the kernel's first-fit search from `0x0000100000000000` | `arena.Init` reserves it with `VMMap` |
| Kernel-chosen mappings | first fit in `[0x0000100000000000, 0x0000700000000000)` | `vm.UserMapBase`/`UserMapLimit`; `region.Map.FindFree` takes the starting point as a parameter |
| Shared-memory window | `[0x0000080000000000, 0x0000081000000000)` | `vm.UserSharedAddress`; the file cache at its start, the 256 KiB exec area 16 MiB in (`exec.AreaAddress`) |
| DMA window | `0x0000081000000000`, four pages | `vm.UserDMAAddress`; `DMAAllocate` refuses any other address |
| PID 1 launch staging page | `0x0000083000000000` | `vm.UserStagingAddress`, mapped by the kernel |
| System image window | `[0x0000082000000000, 0x0000082040000000)` | `vm.UserImageAddress`/`UserImageLimit`; `ImageMap` refuses any other address |
| Shared image frames | one physical copy per read-only segment | keyed by the image's offset in the boot archive (ADR 0081); mapped at the segment's link address |

There is no source of randomness in the kernel. The TSC is read for the clock
(ADR 0054), the kernel does not probe for `RDRAND` or `RDSEED`, it does not use
the UEFI RNG protocol before `ExitBootServices`, and there is no virtio-rng
device.

### What each kind of randomization would need

**Entropy.** Every item below needs a seed before the first process exists.
The candidates are the UEFI RNG protocol, called before `ExitBootServices`
where the firmware provides it, and `RDSEED`/`RDRAND` where CPUID reports
them, with TSC jitter as a weak supplement and never as the only source. A
kernel CSPRNG seeded from these would serve every later choice. Kernel ASLR
needs its entropy even earlier, in the loader stage that picks the slide.

**Kernel ASLR.** This is the largest change. The kernel is linked at a fixed
base, marked non-relocatable (the PE has a `.reloc` section but not
`DYNAMIC_BASE`), and executes normal TinyGo code through the kernel root's low
mapping. Process roots map only the fixed high transition subset. Randomizing
it needs:

- a relocatable kernel: keep base relocations and set `DYNAMIC_BASE`, or have
  the kernel apply its own relocations, and stop reading its header at the
  constant `vm.KernelImageBase` (the loaded-image protocol reports the real
  base);
- a higher-half kernel mapped at a random slide, replacing the fixed transition
  alias and the low-linked execution model, so kernel pointers stop being
  physical addresses;
- no kernel addresses leaked to userspace. `DMAAllocate` returns a physical
  address to the driver, which a device needs; `Diagnostics` in the
  `goos_diag` build prints physical addresses. Nothing else in the native ABI
  returns a kernel address;
- a decision on stronger page-table separation. Process roots already omit the
  low kernel image, but they retain the minimal high transition mappings and
  the shared direct map required by the current entry design.

**Executable ASLR.** Programs are static non-PIE executables. `elf2native`
accepts only `ET_EXEC` and refuses `PT_DYNAMIC`, and the `GOOSNAT` format
carries absolute segment addresses, an absolute entry, and an absolute stack
top, with no relocation table. A per-process base needs:

- position-independent images: a PIE link (`ld.lld -pie`) of TinyGo output.
  TinyGo's support for this on the freestanding userspace target has not been
  tried;
- a relocation table in the native format, applied by the loader to the
  private pages before they are mapped, which keeps W^X: pages are filled
  through the identity map and only then mapped with final permissions. This
  is a new format version; the header has a version field, and reserved fields
  must be zero, so an old loader refuses a new image rather than misreading it;
- the kernel choosing the base per launch and adding it to every segment,
  the entry, and the stack top;
- an image base inside the image area. Page tables are already created on
  demand across the whole user range (ADR 0104), and kernel-chosen mappings
  and the stack sit far above it, so the 47-bit range leaves ample entropy for
  those; images, linked for the small code model, stay below 2 GiB until they
  are position-independent.

**Shared image pages under ASLR.** The frames of ADR 0081 hold code as it was
loaded, and their key is only the archive offset. With position-independent
code the text pages are identical at every base, so sharing still works and
the key needs no change: only the mapping address differs per process.
Anything relocated must not be shared under the same key. Go and TinyGo place
absolute pointers in read-only data (type descriptors, string headers,
interface tables); under PIE these move to relocated read-only data
(`.data.rel.ro`). Those pages would either be private per process, as
writable segments are now, or be shared per `(archive offset, base)`, which
gives up most of the sharing. `loader.Shareable` would have to refuse a
segment that carries relocations.

**Stack randomization.** The initial stack pointer is whatever the image
header says. Randomizing it inside the image's stack reservation needs only a
kernel-chosen offset below `StackTop`, applied in `AddUserTask`; it costs
nothing in the ABI but gives little entropy while the stack sits at a fixed
place in the image. Real stack randomization comes with executable ASLR or
with the kernel allocating the stack as its own region at a random address.

**mmap randomization.** Native userspace has no general `mmap`; Linux
compatibility has bounded anonymous private `mmap` with deterministic first-fit
placement. It refuses a mapping larger than the pages the process quota has
left, so a Linux program meets `ENOMEM` at the call and not a fault at the
first touch (ADR 0147). Randomizing that placement can change the starting search address
without changing its ABI. The three native mapping calls behave differently:

- `MemoryMap` already takes its address from the caller. A process could
  choose a random address inside the shared window today; a randomized or
  larger window only changes the constants. PID 1 and the shell both use
  `exec.PageAddress`, but the exec protocol carries slot numbers, never
  pointers, so the two could map the page at different addresses;
- `DMAAllocate` and `ImageMap` require the caller to pass the fixed address,
  and both write the address they used back to the caller. The kernel could
  choose the address instead without changing the call's shape. The two
  callers, the virtio-blk driver and VFS, check that the returned address
  equals the constant and would have to use the returned value instead.

### Does anything decided so far block it?

No ABI decision blocks randomization. The native-call ABI passes user
addresses as arguments and returns them, and no startup, IPC, lifecycle, or
exec wire format carries a pointer: the `GST4` startup object holds offsets
relative to itself, bytes, and handle IDs, and the exec and launch staging
areas are addressed by slot. The fixed values that would have to change are all kernel constants,
the userspace linker script, and the `DMAAllocate`/`ImageMap` address checks,
and none of them is part of a declared stable ABI.

What does stand in the way is implementation, in this order of cost: the
kernel's fixed, identity-mapped placement; the native image format's lack of
relocations together with the untested PIE build of TinyGo programs; and the
missing entropy source. Two
decisions should keep this open: a new fixed user address should be returned
by the call that maps it rather than assumed by the caller, and a new wire
format should keep carrying slots or offsets rather than pointers.
