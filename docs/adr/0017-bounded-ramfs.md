# ADR 0017: Bounded generation-safe RAMFS

## Status

Accepted

## Decision

The first filesystem uses fixed-capacity node and data storage. Nodes are named
by generation-safe opaque IDs rather than pointers or paths. RAMFS understands
only parent IDs and single path components; the future VFS owns path resolution
and mounts.

All mandatory filesystem operations are implemented with explicit typed status
values. Metadata mutation rejects duplicate names, invalid components,
nonempty-directory deletion, directory cycles, stale IDs, and capacity
overflow. Truncation zeroes removed data.

Service requests use versioned protocol `GRF1`. Inline request data is bounded
by the existing IPC payload. Shared memory remains the intended bulk-data path.

## Consequences

Memory consumption and operation latency have strict bounds, and host tests can
exercise filesystem semantics without UEFI. The current 32-node, one-page-per-
file limits are bootstrap policy, not an on-disk format. VFS, namespaces,
permissions, timestamps, and page-cache integration remain separate work.
