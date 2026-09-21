# ADR 0052: Build-time timer rate and diagnostic build

## Status

Accepted. Replaces the one-line edit described in ADR 0049.

## Context

Timing defects show up at some tick rates and not others, so changing the rate
has to be routine. ADR 0049 named the rate in `kernel/clock` but kept the PIT
reload value as an assembly immediate guarded by a test, so trying another rate
still meant editing Go and assembly by hand.

Finding the cause of the 1 kHz failures (ADR 0053) needed kernel state at the
moment of failure: which handle a wait named, why it was rejected, what each
process held, and which wait registrations were live. Printing this on every
boot would bury the boot log, and adding it by hand for each investigation is
slow and easy to leave behind.

## Decision

`HZ` is a make variable with the values 100 (default), 250, and 1000. It becomes
the `goos_hz250` or `goos_hz1000` build tag, which selects the rate constant in
`kernel/clock`. The kernel passes `clock.PITDivisor` to the timer setup routine,
so the assembly carries no rate. Userspace deadlines already derive from the
same constant and are rebuilt with the same tag.

`DIAG=1` adds the `goos_diag` tag. `diag.Enabled` is a constant, so in a normal
build every recording site and the dump code compile away. A diagnostic build:

- keeps a bounded ring of the last 128 kernel events: wait-any registration,
  immediate readiness, re-check cancellation, completion with the publishing
  object, timeout, consumption, cancellation at task exit, and rejection with
  the failing check; channel creation and closure with the reason; handle
  transfer; process spawn and exit;
- answers native operation 23 with a dump of the scheduler masks, every
  process with its tasks and their state, every handle with its type, rights,
  channel, side, and peer state, every channel with its queues, pending
  requests, and holders, every wait-any registration, wait-set counts, and the
  retained lifecycle queue, followed by the event ring.

Operation 23 requires the console device capability with write rights, the same
authority needed to print the output directly. In a normal build it returns
`Unsupported`. PID 1, VFS, and the block service call it before stopping on a
fatal error, VFS then prints its own session table, and the shell calls it
after `exec: no reply` and from the `diag` builtin.

`build/config/build` records the active values, and every kernel and userspace
target depends on it, so switching `HZ` or `DIAG` rebuilds everything and
repeating a build does not.

## Alternatives

A runtime rate would need the PIT reprogrammed and every deadline computed at
runtime, and userspace deadlines are compile-time constants today. A kernel
log level would leave the tracing code in normal builds and would still need
a way to request the dump.

## Consequences

`make HZ=1000 DIAG=1 test-stress` reproduces and explains a timing failure
without source edits. Normal builds carry no diagnostic code and print only
`Timer: <rate> Hz` in addition to the previous boot log.

The dump exposes other processes' handle tables, so it exists only in builds
made for diagnosis.
