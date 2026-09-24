# ADR 0088: Stack guard gap and frame bound

## Status

Accepted. Extends ADR 0083 and ADR 0006. Replaces ADR 0083's one-page user
stack guard with a 64 KiB gap and settles its "Larger guards or stack probes"
alternative.

## Context

A guard page catches an overflow only if the first access below the stack
lands in it. Pushes and calls write at the stack pointer they have just
lowered, so they cannot step over a guard. A function's frame allocation can:
`sub $n, %rsp` moves the stack pointer by `n` without writing, and the next
write may be anywhere in the new frame or just below it. Compilers close this
hole with stack probes, which touch every page of a large frame from the top.

**Userspace has no probes.** The userspace target is
`x86_64-unknown-none-elf`, for which LLVM emits a probe only when a function
carries the `probe-stack` attribute. TinyGo sets no such attribute and offers
no flag or target field to set it; the target's `-mno-red-zone` and other
`cflags` reach only C sources. The programs do have frames larger than ADR
0083's single guard page: `go tool objdump` shows `SUBQ $0x1aa8, SP` (6824
bytes) in the file service's `userspaceMain`, 5800 bytes in the configuration
service, 5768 in PID 1 and 4856 in the shell's `awaitReply`, and calls that
push up to 2096 bytes of arguments on top of that. A function entered close to
the bottom of the stack could move the stack pointer past the guard page and
write into the heap, the silent corruption ADR 0083 set out to prevent. The
same listings show that Go code uses the System V red zone: functions store
up to 72 bytes below the stack pointer without moving it.

**The kernel's probes do nothing.** The kernel is built for
`x86_64-unknown-windows-gnu`, where LLVM probes every frame of 4 KiB or more.
A scratch program built with the kernel's TinyGo target confirms it: a
3880-byte frame is allocated with a plain `sub`, while 4120- and 9640-byte
frames get `mov $0x25a8, %eax; call ___chkstk_ms; sub %rax, %rsp`. The symbol
links, but to TinyGo's UEFI runtime (`src/device/uefi/asm_amd64.S`), which
defines `___chkstk_ms` as a single `ret`; the kernel's disassembly shows the
same one-instruction body. Every probe is therefore a no-op, and a one-page
kernel stack guard is exactly as unsound as the user one for frames larger
than a page. The kernel has such frames: `main.serviceKernel` allocates 6856
bytes, the validation build's `main.runKernelSelfTests` 17080, and
`kcall.Dispatch`, into which the native-call handlers are inlined and which
runs on the guarded kernel-entry stacks, 3400 bytes in the normal build, 3608
in the validation build and 4456 in the diagnostic validation build. Win64 has
no red zone, and the kernel code has no access below its stack pointer.

## Decision

**A 64 KiB gap below every user stack.** `loader.StackGuardSize` is the single
definition. The userspace linker script leaves exactly that much between the
heap and the stack segment, and a loader test compares the script's line with
the constant. `ParseNative` requires the whole gap to lie in the user range
and to be covered by no segment. `LoadImage` requires it to avoid every
window the kernel maps into a process later (the probe and startup pages below
`0x40002000`, the shared-memory, DMA and system image windows) and to be
unmapped in the address space it loads into. `elf2native` and `service-image`
apply the window check at build time. The gap costs address space only: the
largest image, the file service, now ends at `0x40097000`, well below the
shared-memory window at `0x40180000`. The fixed self-test and fault-probe
layout moves its stack to `0x40030000`, so that the gap stays clear of the
self-test's shared-memory pages at `0x40010000`.

**Every userspace frame is bounded at build time.** `tools/stackframes` runs
`go tool objdump` on every userspace ELF, services and commands alike, before
`elf2native` turns it into an image. For each function it computes the static
depth: how far the stack pointer can get below its value at entry. It follows
control flow, so code after an early return is counted at the depth it runs
at; it counts pushes and pops, `SUBQ` and `ADDQ` by a constant (a negative
constant moves the other way), and `ANDQ $-n, SP` realignment, for at most
`n - 8` bytes and `n` up to 64. Where paths meet at different depths it keeps
the deeper, and jump-table targets are analysed at the depth of the indirect
jump. Anything else that writes SP fails the build with the function's name:
a register or memory amount, `MOVQ`, `LEAQ`, `LEAVE`, `XCHG`, `POPQ SP`, and
so does an access more than the red zone below SP, an undecodable
instruction, and a depth that grows around a loop. No allowlist is needed:
`_start`'s `ANDQ $-0x10, SP` is ordinary realignment, and the kernel-call
stubs in `amd64.S` do not touch SP.

The bound follows from one induction. A function is entered with its return
address just written at its entry stack pointer, so that pointer is at or
above the bottom of the stack unless an earlier write had already faulted;
`_start` starts inside the stack segment. Within the function the stack
pointer stays within its depth below the entry value, and every access is at
most the red zone below the stack pointer. If every depth plus a margin fits
in the gap, the first access below the stack lands in the gap, however deep
the call chain. The margin is the return address a call writes (8 bytes) plus
the red zone (128 bytes), 136 bytes, which is conservative because the return
address slot lies inside the red zone. Callee-saved pushes, pushed arguments
and realignment are part of the depth, not the margin. A function may therefore
be at most 65400 bytes deep. The largest today is 8968 bytes, the file
service's `userspaceMain`: a 6824-byte frame, six saved registers, and 2096
bytes of pushed arguments.

**The kernel gets the same bound, and a two-page guard.** For the kernel's PE
image the tool finds every frame allocation: each `sub $imm32, %rsp` and each
probe sequence, whose `mov` gives the size. `go tool objdump` does not
disassemble TinyGo's PE files, which lack a Go line table, and no other
disassembler belongs to the pinned toolchain, so the scan reads the code
bytes. It fails closed: a byte pattern that merely looks like a frame can fail
the build but never hide a frame, and a call to `___chkstk_ms` that is not
part of the allocation sequence fails it too. The Win64 prologue saves
registers with pushes before it allocates, so the allocation plus the
following call's return address must fit in the guard. The diagnostic build's
4456-byte `kcall.Dispatch` does not fit in one page, so every kernel stack's
guard grows to two pages (`amd64.KernelStackGuardSize`), for a limit of 8184
bytes. `goos_reclaim_exited_task` in `io.S` recomputes a stack's allocation
from its top; a host test compares its constant with the Go side.

Three functions are exempt, each up to a stated size: `main.main` and
`main.serviceKernel` (16 KiB each) and `main.runKernelSelfTests` (24 KiB).
The diagnostic build's `main.main` is 7992 bytes deep. They run only on the
boot task, whose stack is the 64 KiB one TinyGo's runtime allocates from the
heap for the main goroutine, with a canary but no guard page, so the guard
bound does not apply to them.

**The validation build proves a large frame at run time.** The fault probe now
recurses through a function with a 12 KiB frame that writes the lowest word of
each new frame before calling itself. On its four-page stack the first frame
fits and the second one's write lands 8224 bytes below the stack, in the third
page of the gap: past where ADR 0083's single guard page ended. The kernel
counts an exit as a guard fault when the task died of a page fault anywhere in
the 64 KiB below the probe's stack and the faulting page is unmapped, and it
requires at least one such fault more than a page below the stack before it
prints `USER_STACK_GUARD_OK`.

## Alternatives

**Stack probes in userspace.** They would make any gap sound for any frame.
TinyGo exposes neither LLVM's `probe-stack` attribute nor
`-fstack-clash-protection` for Go code, so this needs a TinyGo change.

**Post-processing LLVM IR.** TinyGo can write the program's IR, and a pass could
add `probe-stack` to every function before code generation. The build would
then have to reproduce TinyGo's own code generation and link step - runtime
objects, picolibc, the linker script - with a separately pinned `llc`, and
keep that in step with every TinyGo release. The check is a small host tool
over output the pinned Go toolchain already produces.

**Larger per-program stacks.** A larger stack moves its bottom but leaves the
guard as thin as before; a large frame entered near the bottom steps over it
just the same. It would also cost private memory in every process, where the
gap costs none.

**A gap sized to the largest frame.** 16 KiB would cover today's frames, but
would need raising as programs grow. 64 KiB is seven times the largest depth,
costs no memory, and turns growth past it into a build failure rather than a
silent hole.

**A real `___chkstk_ms` in the kernel.** The kernel cannot define a second one
next to TinyGo's. The linker's `--wrap` could redirect the calls to a real
probe, but that ties the kernel to a linker-driver detail, and the probe would
still not help the boot stack, which has no guard. The check needs neither and
can coexist with real probes later.

## Consequences

A userspace frame, or a kernel frame on a guarded stack, that could step over
its guard now fails the build and names the function. The build prints the
largest frame of every binary; `stackframes -v` lists them all. Userspace
programs have about 56 KiB of headroom, the kernel's `kcall.Dispatch` about
3.7 KiB in the diagnostic build and at least 4.5 KiB otherwise.

The bound is per function, not per call chain. Stack exhaustion is still
detected at run time, by the guard; the bound only makes sure the guard is
where the overflow lands. Code the analysis does not see - pointers derived
from SP in other registers - points into the function's own frame and adds no
depth.

A native call that makes the kernel write into a frame spanning the gap finds
the page unmapped and fails the call, as for any unmapped buffer.

Each kernel stack takes one more physical page, six instead of five.

The boot task's stack, and any other TinyGo goroutine stack in the kernel, has
no guard page at all; an overflow there corrupts the kernel heap. That is a
separate weakness this ADR records but does not fix. ADR 0091 fixes it: the
boot task moves to a guarded stack once paging is up, and the per-function
exemptions above become the bound of that stack's guard.
