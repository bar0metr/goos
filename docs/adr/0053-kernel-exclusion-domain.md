# ADR 0053: One kernel exclusion domain for native calls and housekeeping

## Status

Accepted. Its diagnosis is corrected by ADR 0056: the corrupted wait interests
described below were SSE register state lost across preemption, not aliased
physical pages. The races this record closes are real and the decision stands,
but they were not shown to cause the observed failures.

## Context

At 1 kHz the VFS service intermittently stopped with
`VFS_WAIT_PERMISSION_DENIED`, often on its first `WaitAny` right after
`FAT32_MOUNT_OK`, and PID 1 intermittently stopped with
`INIT_LIFECYCLE_FAILED` or left the shell with `exec: no reply`. A parallel
QEMU stress run reproduced these in 4 of 8 guests.

The diagnostic build (ADR 0052) recorded the rejected interest. VFS had asked
to wait on handle `0x4000000000000005` while its own variables still held the
correct session and control handles (`0x100000002`, `0x100000003`). That value
is not a handle: it is the object identifier of channel 5, the shell's exec
channel, which only kernel memory contains. The VFS heap page holding the
interest array was also in use as kernel memory, and a kernel stack frame from
the shell's exec request had overwritten it. VFS never held a stale session
handle.

The cause was the kernel's own task. Native calls enter through an interrupt
gate, so they run with interrupts disabled and assume they own every kernel
structure. The kernel loop that delivers exits, launches programs, expires
deadlines, and retries lifecycle messages ran ordinary Go code with interrupts
enabled, and the timer preempts any task. Every structure the loop mutated
could therefore be observed half-updated by a native call:

- `pmm.Allocator.Allocate` scans the bitmap and then marks the page used. A
  launch preempted between the two while the virtio driver allocated DMA pages
  handed one physical page to two owners, which is the aliasing observed above.
- `process.Registry.Remove` decrements the count before moving the last entry
  into the hole, so a native call issued in that window cannot find its own
  process and fails with `PermissionDenied`.
- Handle tables, the channel registry, wait sets, and the lifecycle queue had
  the same exposure.

The demo task that expired deadlines had the same defect. A higher timer rate
raised the preemption frequency tenfold, which is why the failures clustered at
1 kHz and at boot, where launches overlap driver initialization.

## Decision

The kernel has one uniprocessor exclusion domain: interrupts disabled. Native
calls already run in it. Kernel housekeeping now runs each step in it too:
`serviceKernel` disables interrupts, expires deadlines, flushes retained
lifecycle messages, delivers exits, services one launch request, and restores
the previous state. The boot-time installation of the first user tasks runs in
it as well, and the demo task no longer touches wait state.

The rule is that kernel code running in task context must enter the domain
before touching shared kernel state. Interrupt handlers and native calls are
already inside it.

## Alternatives

Retrying the wait in VFS, or reordering `Registry.Remove`, would each hide one
symptom and leave the allocator aliasing pages. Fine-grained locks are the
eventual SMP design but add complexity with no benefit on one CPU.

## Consequences

The 1 kHz stress passes 12 of 12 guests where it previously failed 4 of 8, and
the failure signatures above are gone.

While a housekeeping step runs, timer interrupts are held pending. A step that
takes longer than one tick coalesces ticks, so time advances slightly slower
during program launch. ADR 0057 measures this cost.

SMP needs real locks. The exclusion domain marks where they will be needed:
the shared structures a native call or housekeeping step touches.
