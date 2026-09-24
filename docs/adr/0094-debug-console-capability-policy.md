# ADR 0094: No identity-authorized console output

## Status

Accepted. Retires the DebugWrite operation of ADR 0009; amends ADR 0089's table
of console writers.

## Context

Native operation 2, `DebugWrite`, wrote bytes to the serial console. It was the
first operation the kernel had (ADR 0009), from before handles existed, and it
was authorized by comparing the caller's address-space root with two bootstrap
processes: PID 1 and the validation self-test process A. No capability named
it. ADR 0009 already said it would be removed or capability-restricted once a
userspace terminal existed; the terminal has existed since ADR 0082.

In a normal build PID 1 was its only user. PID 1 already holds the console
device capability with Write, which it grants to services, and the
capability-checked `ConsoleWrite` operation does exactly what `DebugWrite` did.
The identity check was therefore the only thing `DebugWrite` added: a piece of
ambient authority tied to who the caller is rather than to what it holds.

## Decision

**`DebugWrite` is retired in every build.** The dispatcher has no case for
operation 2, so it returns `Unsupported` like any unknown number, whatever its
arguments, and the kernel handler is gone. The number is kept reserved
(`kcall.OperationRetiredDebugWrite`) so it is never reused for something a
stale binary might call by mistake. `goos_debug_write` is gone from the
userspace runtime.

**PID 1 prints through its console capability.** Its `write` helper calls
`ConsoleWrite` with the console device handle (`0x100000002`, Read, Write and
Transfer), the same authority it hands to services. What PID 1 prints and when
is unchanged (ADR 0089): the boot banner, `Rebooting...`, validation markers.

**The self-test process holds a capability too.** Process A is given a console
handle with Write only. Its ring 3 program first calls the retired operation and
requires `Unsupported`, then submits an invalid pointer through `ConsoleWrite`
and requires `InvalidArgument`, then prints `USER_CONSOLE_CAP_OK`, and finally
requires `PermissionDenied` for its console slot with a forged generation. Any
other answer executes `ud2`, the process never exits cleanly, and the boot test
fails. Process B no longer calls operation 2 at all; its calls were denied and
unchecked.

**What the kernel still learns from PID 1's output is bookkeeping.** The
validation boot waits until PID 1 has printed and has reported
`USERSPACE_SUPERVISOR_OK`. `ConsoleWrite` notes both in the validation build
after the write was authorized by the capability. It grants nothing.

## Alternatives

**Compile `DebugWrite` only into validation builds.** Acceptable to the
specification, but the validation build would still carry an identity-based
path that nothing needs, and the release and validation ABIs would differ in an
operation's semantics rather than in which operations exist.

**Give `DebugWrite` a handle argument.** It would become `ConsoleWrite` under
another number.

**A separate debug-console capability.** A handle whose only right is writing
diagnostics, distinct from the console, would let a future build separate the
two. Nothing distinguishes them today: every writer of diagnostics already
holds the console, and the log (`LogWrite`) is the structured path for
diagnostics. It can be added when a process needs one without the other.

## Consequences

No native call authorizes by comparing the caller with a bootstrap process. The
remaining identity rules (only PID 1 may create channels or reach the launch
path) are about which process holds which kernel endpoint, not about console
output, and are unchanged.

The host test `TestDispatchRefusesRetiredDebugWrite` proves the dispatcher
refuses operation 2 without reaching a handler, in every build. The boot test
requires `USER_CONSOLE_CAP_OK`, which process A prints only after the retired
call was refused. `USER_COPYIN_OK` now comes from `ConsoleWrite`'s rejection of
the invalid pointer.

The attack surface loses one operation and one static-buffer consumer; the
4096-byte console bound is renamed `kcall.MaxConsoleWrite`.
