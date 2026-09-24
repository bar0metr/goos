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

Every image's stack segment has an unmapped guard page directly below it
(ADR 0083). The loader rejects an image with a segment in that page, and
TinyGo images place their stack in its own segment one page above the heap.
A stack overflow is a page fault that retires the process.

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
layout. A higher-half kernel, guard regions beyond the stack's, demand paging, and ASLR
remain later work.

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
| Kernel identity map | physical = virtual for `[1 MiB, 1 GiB)` | `vm.BuildIdentity`; kernel stacks, heap arena, page tables, and the boot archive are all reached at their physical address |
| User pointer range | `[0x40000000, 0x80000000)` | `vm.ValidateUserRange` (`IdentityMapLimit`..`UserAddressLimit`) |
| Initial user page tables | one leaf table for `[0x40000000, 0x40200000)` | `vm.CreateUserAddressSpace`; `ExtendUserAddressSpace` can add one more leaf table per two-MiB region in the same GiB |
| Startup page | `0x40001000` | `vm.UserStartupAddress`; read only through `StartupRead` |
| Image base (code, then rodata, data, bss, stack, heap) | `0x40002000` | `userspace/runtime/amd64.ld`; images are static `ET_EXEC` files |
| Initial stack pointer | `_stack_top` in the image's data segment | `StackTop` in the native header, taken from the linker symbol by `elf2native` |
| Userspace heap arena | `_heap_start`..`_heap_end` in the image | linker script; each program's bump allocator uses it |
| Shared-memory window | `[0x40180000, 0x401c0000)` | `vm.UserSharedAddress`/`UserSharedLimit`; the exec page is mapped at its start (`exec.PageAddress`) |
| DMA window | `0x401c0000`, four pages | `vm.UserDMAAddress`; `DMAAllocate` refuses any other address |
| PID 1 launch staging page | `0x401ff000` | `supervisor.StagingAddress`, mapped by the kernel |
| System image window | `[0x40200000, 0x40600000)` | `vm.UserImageAddress`/`UserImageLimit`; `ImageMap` refuses any other address |
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
`DYNAMIC_BASE`), and runs identity-mapped in the same low GiB that every
process shares through its cloned kernel PDPT entry. Randomizing it needs:

- a relocatable kernel: keep base relocations and set `DYNAMIC_BASE`, or have
  the kernel apply its own relocations, and stop reading its header at the
  constant `vm.KernelImageBase` (the loaded-image protocol reports the real
  base);
- a higher-half kernel mapped at a random slide, with the physical-memory map
  moved out of the identity range and placed at its own random offset, so that
  kernel pointers stop being physical addresses;
- no kernel addresses leaked to userspace. `DMAAllocate` returns a physical
  address to the driver, which a device needs; `Diagnostics` in the
  `goos_diag` build prints physical addresses. Nothing else in the native ABI
  returns a kernel address;
- separation of kernel and user page tables (KPTI or equivalent) before the
  slide is worth much, because timing side channels can recover a kernel
  mapped in every address space.

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
- page tables allocated on demand across the user range. Today a new address
  space has one leaf table, so an image must fit in
  `[0x40000000, 0x40200000)`. Randomizing inside the one-GiB user range at
  page granularity gives at most 18 bits of entropy, fewer after alignment and
  the fixed windows are excluded. A larger user range would need a wider
  `ValidateUserRange` bound.

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

**mmap randomization.** There is no general `mmap`. The three mapping calls
behave differently:

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
exec wire format carries a pointer: the `GST3` startup block holds lengths,
bytes, and handle IDs, and the exec and launch staging pages are addressed by
slot. The fixed values that would have to change are all kernel constants,
the userspace linker script, and the `DMAAllocate`/`ImageMap` address checks,
and none of them is part of a declared stable ABI.

What does stand in the way is implementation, in this order of cost: the
kernel's fixed, identity-mapped placement; the native image format's lack of
relocations together with the untested PIE build of TinyGo programs; the
single preallocated user leaf table; and the missing entropy source. Two
decisions should keep this open: a new fixed user address should be returned
by the call that maps it rather than assumed by the caller, and a new wire
format should keep carrying slots or offsets rather than pointers.
