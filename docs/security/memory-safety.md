# Memory safety

GoOS is written in Go and compiled with TinyGo. Go removes most memory-safety
defects from ordinary code; the remaining risk sits where the kernel touches
raw addresses, hardware state, and user memory. This document lists where that
happens and which rules keep it contained.

## Where `unsafe` is allowed

`make lint` fails if any `.go` file under `kernel/` or `internal/` imports
`"unsafe"` outside directories named `uefi`, `vm`, or `amd64`:

```sh
grep -R --include='*.go' -n '"unsafe"' kernel internal \
  --exclude-dir=uefi --exclude-dir=vm --exclude-dir=amd64
```

Current kernel users:

| Package | Use |
| --- | --- |
| `kernel/boot/uefi` | UEFI protocol calls, memory-map descriptors, initramfs slice |
| `kernel/mm/vm` | page-table walks, identity-mapped physical access, kernel image header |
| `kernel/arch/amd64` | task context and kernel stacks, TSS descriptor, LAPIC/IOAPIC MMIO |

All other kernel packages (`handle`, `ipc`, `stream`, `process`, `loader`,
`ramfs`, `vfs`, `wait`, `kcall`, ...) are plain Go and are tested on the host.

Userspace commands under `userspace/cmd/` also use `unsafe`, for native-call
stubs, arena allocators, and wire buffers. The lint rule does not cover them;
the kernel treats everything they pass as untrusted.

## SAFETY comments

Every `unsafe` conversion in the kernel should carry a `// SAFETY:` comment
directly above it that states the invariant making it sound: who owns the memory, why it
is mapped, and why the size is bounded. Example from `kernel/mm/vm`:

```go
// SAFETY: SizeOfImage was read from a bounded header and capped at 16 MiB.
```

A new `unsafe` use without such a comment should not pass review. Not every
existing site follows this yet: the UEFI protocol calls in
`kernel/boot/uefi/initramfs.go` and the `mmioRead32`/`mmioWrite32` helpers in
`kernel/arch/amd64/tables_uefi.go` have no SAFETY comment.

## W^X and NX

- Boot fails if the CPU lacks NX. The kernel enables `EFER.NXE` and `CR0.WP`
  before loading its own page tables.
- Kernel PE sections map to leaves by flags: executable sections read-only,
  writable sections NX. A section requesting write and execute is rejected.
- Identity-mapped memory outside the image is writable and NX. The first MiB
  is unmapped.
- User leaves reject W+X at map time. The native loader rejects writable
  executable segments and fills pages through the kernel identity map before
  installing the final user mappings (docs/architecture/native-images.md).
- Shared-memory and DMA mappings are always NX.
- `MemoryMap` installs shared pages only inside the shared window
  `[0x40180000, 0x401c0000)` and only after every target leaf is free. The
  leaves are not owned, so an exiting process never frees shared storage
  (ADR 0063).

## Guard pages

- Every kernel-entry stack is four pages with an unmapped guard page below it
  (`kernel/arch/amd64/scheduler_uefi.go`). Overflow faults instead of
  corrupting the neighbouring allocation.
- User address space starts at 1 GiB; low addresses are unmapped in every
  process, so null dereferences fault.
- Every user stack has an unmapped guard page directly below it (ADR 0083).
  The userspace linker script gives the 32 KiB stack its own RW+NX segment one
  page above the heap. The loader enforces the gap for every image, not only
  those built by that script: `ParseNative` rejects an image whose stack
  segment has another segment in the page below it or starts within one page
  of the user range, and `LoadImage` rejects a guard page the kernel would
  map later (the startup page and the shared, DMA and system image windows).
  An overflow therefore takes a page fault, which retires the process like any
  other user fault; the kernel does not panic. The validation build's
  fault-probe overflows its stack on every run, and the kernel requires a
  not-present fault in its guard page (`USER_STACK_GUARD_OK`).

## User pointers

The kernel never dereferences a user virtual address. Native calls use
`vm.CopyIn`, `vm.CopyOut`, `vm.CanCopyOut`, and `vm.ValidateUserRange`:

- the range must lie within `[1 GiB, 2 GiB)` and must not wrap;
- every page crossed must be present and user-accessible at all four levels;
- `CopyOut` additionally requires a writable leaf;
- data moves through the kernel identity map after validation.

Calls that both consume object state and write results validate the output
buffer first. `StreamRead`, for example, checks `CanCopyOut` before removing
bytes from the ring, so a bad buffer cannot lose data.

## Zeroed memory

- Page-table pages and user leaves are cleared before exposure.
- The loader zero-fills memory beyond each segment's file bytes.
- Shared-memory objects and the DMA window are zeroed on allocation.
- RAMFS truncation clears discarded bytes.
- The freestanding userspace arena allocators clear every allocation, which
  restores the zeroing guarantee the Go compiler relies on (ADR 0048).
- A reused scheduler slot starts from a clean FXSAVE image (ADR 0056).

## Kernel exclusion domain

The kernel has one exclusion domain on its single CPU: interrupts disabled
(ADR 0053). Native calls enter through an interrupt gate and run inside it.
Kernel housekeeping (`serviceKernel`) disables interrupts for each step:
deadline expiry, lifecycle retry, exit delivery, and one launch. Kernel code in
task context must enter the domain before touching the allocator, registries,
handle tables, wait sets, or the lifecycle queue. Without this rule a preempted
update exposes torn state, for example a physical page handed to two owners.
SMP will need real locks; the domain marks where.

## Task context includes SSE

TinyGo copies arrays through XMM registers, and the kernel's Go code keeps
XMM6–XMM15 live across calls. Each scheduler slot owns a 512-byte FXSAVE area,
saved on every switch away from a task that will run again and restored on
switch-in (ADR 0056). Without it, one task observes another task's or the
kernel's data, including channel identifiers. Enabling AVX would require XSAVE.

## Bounded buffers and structures

Kernel buffers for user data are static and sized by the contract, not by the
caller:

| Buffer | Size | Bound enforced by |
| --- | --- | --- |
| debug and console write | 4096 B | `kcall.MaxDebugWrite` |
| IPC wire | 320 B | `ipc.WireMaxSize` |
| stream transfer | 256 B | `kcall.MaxStreamIO` |
| `WaitAny` interests | 16 × 16 B | `kcall.WaitAnyMaxObjects` |
| startup block | 1024 B | `kcall.StartupSize`, one read-only page per process |
| launch staging | 4 × 1024 B | `supervisor.StagingSlots`, copied once per launch |

Tables are fixed arrays: 128 handles per process, 32 processes, 64 tasks,
16 channels with 16-message queues, 16 streams with 4 KiB rings, 64 waiters
per wait set. Exhaustion returns `ResourceExhausted` or `PermissionDenied`.

## The kernel heap is boot-only

TinyGo builds the kernel with `gc: leaking`, so nothing the kernel allocates is
ever freed, and the 64 MiB arena cannot grow once the kernel has left boot
services: `runtime.growHeap` faults and the machine dies with no output. An
allocation on a repeating path is therefore a remote denial of service, reached
by running enough commands. Kernel code reachable from a native call, from
`serviceKernel`, or from a process launch or exit must not allocate: it uses
shared fixed buffers, in-place `Reset`/`Init` instead of returning large
structures by value, and `[]byte` rather than string conversions. ADR 0065
states the rule and the two checks that enforce it — the `runtime.alloc` caller
list in the built image, and `runtime.heapptr` read from the QEMU monitor
before and after a batch of commands.

## No panics on untrusted input

Native calls return a `kcall.Status`; they do not panic or halt on bad
arguments. The kernel halts only when a boot step fails or an internal
invariant breaks, such as a wake naming a stale task. The integer checks that
matter (length limits, `offset + width`, `now + timeout` wraparound, address
range wraparound) are listed per operation in
[attack-surface.md](attack-surface.md).

Userspace services stop with a fatal marker on conditions they treat as
unrecoverable, for example a failed receive on a service endpoint. The
supervisor restart policy decides what happens next.
