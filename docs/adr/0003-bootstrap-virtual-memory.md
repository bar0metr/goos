# ADR 0003: Four-level bootstrap virtual memory

Status: Accepted

## Context

After `ExitBootServices`, the kernel still runs on page tables created by UEFI.
Their ownership and permissions are outside the kernel's security model. The
physical allocator currently manages the first GiB on a 512 MiB QEMU machine.

## Decision

Construct kernel-owned four-level x86_64 tables with 4 KiB pages and identity
map the supported first GiB. Leave the first MiB unmapped. Default mappings are
supervisor RW+NX. Parse the in-memory, fixed-base PE image and replace its leaf
permissions from validated section characteristics. Reject every W+X request.
Enable EFER.NXE and CR0.WP before loading the new PML4 into CR3.

## Alternatives

Retaining firmware tables provides no enforceable ownership or permission
contract. Two-MiB huge pages cannot represent the current PE section boundaries
without granting write and execute to the same leaf. Moving immediately to a
higher-half layout complicates the handoff before exception handling exists.

## Consequences

The kernel owns its mappings and hardware enforces NX, read-only supervisor
pages, and W^X. The mapping consumes roughly two MiB of page-table memory and is
limited to one GiB. Per-process tables, higher-half relocation, guard pages, and
sparse physical mappings require later revisions.
