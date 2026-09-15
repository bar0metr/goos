# ADR 0016: Capability-isolated userspace TTY

## Status

Accepted

## Decision

TTY policy lives in a native userspace service. Applications communicate with
it through protocol `GTY1` version 1 over ordinary typed IPC. The protocol
carries bounded write/input data and explicit echo/clear control operations.

COM1 remains a kernel-owned device. A single process-local `Device` handle with
`Write` rights is granted to the bootstrap TTY service. The console kernel call
requires that exact capability and rejects other processes, forged handles,
invalid mappings, empty writes, and oversized writes.

The bootstrap executable images use fixed handle slots only as a test fixture;
startup aborts if the assigned handles differ. The future init service will pass
startup handles through an explicit process bootstrap block.

## Consequences

Clients cannot bypass the service and write the UART. TTY parsing and line
editing are independently host-testable and allocation-free. Serial input IRQ
handling and interactive shell integration remain later service work.
