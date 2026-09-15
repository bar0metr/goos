# ADR 0024: Spawn capability manifests

## Status

Accepted

## Decision

Every supervised launch request may carry up to seven capability grants. A
grant contains a PID 1 source handle, its expected object type, and the exact
rights requested for the child. The lifecycle protocol carries no raw kernel
object identifiers.

Before allocating the child address space, the kernel resolves every source
handle with Transfer authority and verifies that the requested mask is a strict
derivation of existing authority. It then installs the derived handles in
manifest order into the empty child table. Partial installation is rolled back
on failure, and the source handles remain valid.

## Consequences

PID 1 can define a child's initial authority without asking the kernel to mint
rights from untrusted numeric object identifiers. Forged handles, wrong types,
missing Transfer authority, rights escalation, malformed records, and oversized
manifests are rejected.

The initial handle numbers are deterministic because a new process table is
empty and grants are installed in manifest order. A named startup-handle
directory can replace this positional convention when the process ABI grows.
