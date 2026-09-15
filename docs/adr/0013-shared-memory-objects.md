# ADR 0013: Capability-controlled shared-memory objects

Status: Accepted

## Context

Typed IPC bounds inline payloads to 256 bytes. Copying large data through every
message would violate the bulk-transfer architecture. Raw physical addresses
cannot be exposed because they bypass process-local authority and ownership.

## Decision

Introduce shared-memory objects containing up to 16 zeroed physical pages.
Allocate nonzero monotonically increasing object identities through a registry
in a namespace disjoint from bootstrap physical-page identities.
Represent access only through process-local memory handles with Read, Write, and
Transfer rights; Execute is never accepted.

Map an object into a process only after validating its handle, object identity,
page range, virtual range, and every destination leaf. Receiver mappings may be
read-only even when the sender retains write authority. All mappings are NX.

## Alternatives

Embedding bulk bytes in IPC multiplies copies and queue storage. Passing raw
physical pages creates ambient authority. Mapping shared pages executable would
undermine W^X and requires a separate write-then-seal lifecycle.

## Consequences

Two processes can map the same physical pages with different rights and exchange
data without kernel payload copies after setup. Objects and mappings are bounded.
Unmapping, reference counting, reclamation after the last handle closes, quotas,
and a future executable seal API remain separate lifecycle work.
