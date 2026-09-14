# ADR 0007: Process address spaces and local handles

Status: Accepted

## Context

Userspace isolation requires Process to own resources independently from the
Task that executes them. Global resource identifiers would create ambient
authority and make stale-object reuse difficult to detect.

## Decision

Define Process as the owner of one page-table root, one local handle table, and
a bounded set of Task IDs. Encode handles as generation plus local slot index,
record an explicit object type and rights mask, and require non-escalating
derivation for transfer. Build a separate PML4/PDPT for every process while
sharing only supervisor kernel mappings.

## Alternatives

Global kernel object IDs allow cross-process guessing and need a separate
authorization lookup. Raw pointers expose layout and lifetime. Sharing one page
table with software-only ownership does not provide hardware isolation.

## Consequences

Processes can name only handles installed in their own table, stale handles are
detected, and two process roots can resolve one virtual address differently.
Tables are currently fixed-capacity and address spaces do not reclaim page-table
pages. Ring 3 execution, teardown, copyin/copyout, and process wait semantics
remain future work.
