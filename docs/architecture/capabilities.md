# Capabilities and handles

Kernel objects are never exposed as global pointers. Each Process owns a fixed
table of typed opaque handles. A handle encodes a one-based slot index and a
generation. Closing a handle clears the object and advances the generation, so
stale and forged identifiers fail lookup even when the slot is reused.

Every slot records an object identity, object type, and capability rights. A
lookup succeeds only when the identifier, generation, type, and complete
required-rights mask match. Zero identifiers, invalid types, empty authority,
and requests beyond the bounded table capacity are rejected.

Capability transfer requires explicit Transfer authority on the source handle.
The destination receives a new process-local handle and a derived rights mask
that cannot exceed the source rights. Transfer does not revoke the source;
move semantics will be defined by the IPC protocol rather than hidden inside
the handle primitive.

PID 1 service definitions identify source handles, expected object types, and
the exact rights to derive. Lifecycle launch requests serialize at most seven
such entries. The kernel resolves every source in PID 1's table with Transfer
authority before process creation, rejects rights escalation, and rolls back
all installed child handles if any derivation fails. Child handle slots follow
manifest order in the initially empty process table.

Host-side security tests cover wrong types, missing rights, stale identifiers,
forged generations, unauthorized transfer, rights escalation, and capacity
exhaustion. The boot path transfers a read-only memory capability between two
Process tables and launches supervised processes with a derived device
capability that omits PID 1's Transfer right.
