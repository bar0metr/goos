# ADR 0018: Namespace-scoped userspace VFS

## Status

Accepted

## Decision

VFS is a userspace service boundary above filesystem backends. A namespace owns
its mounts, path cache, and open-file objects. Backends expose node operations;
they do not receive full paths. Longest component-prefix selection routes each
request to a mount without ambiguous string-prefix matches.

Open objects use generation-safe handles with explicit read/write rights and
per-open offsets. All tables and paths have fixed bounds. Metadata changes and
mount changes invalidate the resolver cache before returning.

Service requests use versioned protocol `GVF1`. Inline data remains deliberately
small; shared memory is the bulk path planned by the filesystem architecture.

## Consequences

Namespaces can present independent filesystem views without kernel pathname
logic. Cross-mount rename is rejected because no atomic backend contract exists.
The current cache uses coarse full invalidation; finer dependency tracking is a
later measured optimization. Permissions, timestamps, symlinks, and persistent
backends remain out of scope for this slice.
