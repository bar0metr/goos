# ADR 0061: Release shell commands and system reset

## Status

Accepted. Completes the shell command set of spec §24.

## Context

Spec §24 lists `services`, `handles`, `caps`, and `reboot` among the minimal
shell commands, and §57 shows `ps` with type and state columns. The shell must
use public system interfaces rather than reading kernel structures. The M7
acceptance writes a file, reboots from the shell, and reads the file back.
Until now the reboot-persistence test started a second VM instead.

## Decision

Kernel operations:

- `HandleInfo` (27) describes one slot of the caller's own handle table: the
  handle ID, type, and rights. It never reveals the kernel object behind a
  handle.
- `ProcessInfo` (28) lists live processes: PID, scheduling state (running,
  ready, waiting), task count, and the executable's base name, which the kernel
  now records at spawn. Like `SystemInfo` it needs no capability. The exposure
  is limited to names and states.
- `SystemReset` (29) resets the machine through the chipset reset register,
  then the keyboard controller, then a forced triple fault. It requires a new
  device capability, the system-control object, which the kernel grants only to
  PID 1 at boot, at the fixed handle `0x100000012`.

Shell requests that launch nothing use a separate control protocol,
`GCTL001` version 1, on the existing exec channel:

- `services` returns up to eight supervised services with state, PID, and
  restart count.
- `reboot` makes PID 1 send a sync request to VFS, a new VFS control operation
  (control version 3) that flushes the block device. PID 1 then calls
  `SystemReset`. A failed sync or a refused reset is answered, so the shell
  reports it instead of hanging.

Shell builtins:

- `services` prints the supervisor's service table.
- `handles` prints the shell's own handle table.
- `caps` prints the shell's handles under the names they were granted with.
- `reboot` restarts the machine as described above.
- `log` prints the system log (ADR 0059).

The `ps` utility prints `PID TYPE STATE COMMAND`, with type `native` for every
process today.

`write PATH TEXT...` writes its arguments instead of standard input, as in the
spec's M7 script. The 128-byte startup block limits how long the path and
text can be.

## Alternatives

Rebooting from the kernel on any process's request would give every process
ambient authority over the machine. Letting the shell read PID 1's supervisor
state from shared memory would bypass the typed protocol.

## Consequences

The validation boot test runs the new commands and adds an M7 phase inside one
VM: write, `reboot`, read back after the second boot.

The system-control capability is the first authority that can stop every other
component. Only PID 1 holds it, and it is never transferred.
