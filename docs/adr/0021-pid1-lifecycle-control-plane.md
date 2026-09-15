# ADR 0021: PID 1 lifecycle control plane

## Status

Accepted

## Decision

PID 1 receives a dedicated process-local channel capability for lifecycle
control. The versioned `GINITLIF` protocol carries process-exit notifications,
correlated launch requests, and launch results. Endpoint ownership is resolved
by a bounded table keyed by channel object and process address-space root; a
forged handle or a handle from another process cannot select an endpoint.

Architecture exit bookkeeping enters the process registry. The bootstrap
control plane converts a final-Task exit to a lifecycle notification and wakes
PID 1. Init decides to issue a launch request. The kernel validates the request,
creates a fresh address space, reloads the service native image, registers a
new Process and Task, and replies on the original correlation.

## Consequences

Service restart is no longer initiated directly from the kernel polling loop:
it requires an authorized PID 1 request and produces a correlated result.
Endpoint storage is fixed at two endpoints for every bounded channel. The
current launch backend supports the bootstrap crash-probe image; resolving
arbitrary service executables and running the complete policy state machine in
PID 1 remain subsequent work.
