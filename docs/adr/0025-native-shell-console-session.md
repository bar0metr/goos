# ADR 0025: Native shell console session

## Status

Accepted

## Decision

The first shell is an independent TinyGo native image supervised by PID 1. Its
spawn manifest derives one serial device handle with `Read|Write` rights and no
Transfer authority. Console output no longer recognizes a hard-coded bootstrap
process; both input and output resolve the caller's process-local device handle.

The native call ABI adds nonblocking single-byte console input and an
eight-byte monotonic tick read. The shell uses the existing bounded line editor
and implements allocation-free command parsing for `help`, `echo`, `clear`, and
`uptime`. Bootstrap `DebugWrite` is restricted to PID 1 and the ABI probe.

## Consequences

The ISO now boots into an interactive shell whose command path is exercised by
real UART input in QEMU. The shell has no global UART privilege beyond its
derived handle and cannot delegate that handle.

This console-session ABI is intentionally transitional. A persistent userspace
TTY server must eventually own the device and multiplex sessions through IPC.
