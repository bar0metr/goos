# ADR 0091: Guarded boot stack and a stack for double faults

## Status

Accepted. Extends ADR 0088.

## Context

ADR 0088 put guards below every kernel stack the kernel allocates, but not
below the one the boot task runs on. That stack is the 64 KiB goroutine stack
TinyGo's runtime allocates from the kernel heap for `main`, with a canary and
no guard. Everything after early boot ran on it: the rest of boot, the
validation self-tests, and the service loop the boot task ends in. An overflow
there would have overwritten the kernel heap below it. `tools/stackframes`
exempted the three functions that live on it from the kernel frame bound and
gave each an arbitrary size of its own.

The guards that did exist did not produce a report either. When a kernel stack
runs into its guard, the page fault cannot push its frame onto that stack, so
the CPU raises a double fault, and the double fault is delivered on the same
exhausted stack. The third fault resets the machine. With `-no-reboot` QEMU
simply exits: no panic, no address, nothing on the console.

## Decision

**The boot task moves to a guarded stack as soon as paging is up.** After the
kernel's page table is loaded, `amd64.RunOnBootStack` allocates
`BootStackSize` (64 KiB) of contiguous pages with `BootStackGuardSize`
(32 KiB) of unmapped guard below them and calls `goos_switch_to_boot_stack`.
This short assembly routine loads the new stack pointer, aligns it and leaves
the 32 bytes of home space Win64 requires, then calls `goos_boot_continue`,
which is the rest of what `main` used to do. Nothing returns to the old stack.
Only the early boot runs on it: console, initramfs, UEFI exit, PCI discovery,
the page allocator and the page table.

Leaving TinyGo's goroutine stack behind is safe here. The kernel uses the
leaking collector, which scans no stacks. The kernel has no goroutines,
channels or timers, so the boot task never yields to TinyGo's scheduler, and
no runtime code looks at the goroutine's recorded stack again.

**The boot stack's guard bounds the boot task's own functions.** The guard is
wider than the 8 KiB of the other kernel stacks because the self-tests have
frames of up to 17 KiB. `tools/stackframes` now holds `goos_boot_continue`,
`main.serviceKernel` and `main.runKernelSelfTests` to the boot stack's guard
less the return address, 32760 bytes, rather than to sizes picked per function.
`main.main` runs before the switch, so it is bounded like any other kernel
function.

**A double fault gets its own stack.** `InstallDescriptorTables` allocates one
page with the usual two-page guard, sets it as the TSS's first interrupt stack,
and gives vector 8 IST 1. The fatal path recognises the vector and prints
`PANIC: double fault, a kernel stack overflow if this address is a guard
page: 0x…` followed by CR2. CR2 still holds the address of the page fault that
could not be delivered, so it names the guard page that stopped the overflow.

**The validation build proves it on the running system.** The boot test has a
new phase. It sends `fault kernel-stack` to the shell. The shell passes the
request to PID 1 as `ControlFault` with `FaultKernelStack`, and PID 1 makes the
new native call `ValidationFault` (35) with its system-control capability. The
kernel prints the guard range of the calling task's kernel-entry stack, then
recurses through 4 KiB frames until the guard stops it. The test requires the
double-fault report, requires its address to lie inside the printed guard
range, and requires that the machine booted only once. In the first run, the
guard was `[0x57f000, 0x581000)` and the address was `0x580ff8`: the return
address the next call tried to push just below the stack. The boot marker
`BOOT_STACK_GUARD_OK` also checks that, after the switch, the boot task runs
inside its stack and that every page of the guard is unmapped.

## Alternatives

**Changing TinyGo's runtime to allocate `main`'s goroutine stack with a
guard.** This would need a patched TinyGo, and the pinned toolchain (ADR 0086)
is an unmodified release.

**Guarding the goroutine stack where it is.** The stack is a heap allocation
that is not page-aligned, and the page below it holds other heap objects, so
unmapping that page would break them.

**Checking a canary.** TinyGo already places one, but it is checked only on
goroutine switches. The boot task makes none, and a canary finds damage only
after it is done.

**IST for the page fault as well.** A page fault on its own stack would be
delivered even when the faulting stack is exhausted. But every user page fault
would then run on that one stack, and nested faults would reuse it. The double
fault is where exhaustion ends up anyway, and it is an abort that needs no
return.

## Consequences

Every kernel stack now has a guard, and a kernel stack overflow ends in a
report that names the guard page instead of a silent reset. The boot stack and
its guard take 96 KiB, and the double-fault stack takes three pages, all from
the page allocator at boot. The goroutine stack TinyGo allocated for `main`
stays allocated; the kernel heap never frees anyway (ADR 0065).

`ValidationFault` answers `Unsupported` in every build except the validation
build. There it requires the system-control capability, which only PID 1
holds, and a successful call ends the machine.
