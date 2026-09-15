# ADR 0019: Bounded init supervision

## Status

Accepted

## Decision

Init uses a fixed-capacity service table with declaration-ordered dependencies,
explicit capability manifests, typed health states, and `never`, `on-failure`,
and `always` restart policies. Process creation remains behind a launcher
boundary. Exit notifications drive supervision; restart attempts have a fixed
window, backoff, and limit. Diagnostics use a bounded event ring.

## Consequences

Configuration and crash loops cannot grow kernel or service memory without
bound. Dependencies are acyclic by construction because a service may refer
only to definitions already registered. A failed dependency degrades running
dependants without silently killing them. Actual initramfs discovery, PID 1
launch, process-death IPC, and resource reclamation remain separate integration
work; the current boot path validates policy rather than claiming those paths.
