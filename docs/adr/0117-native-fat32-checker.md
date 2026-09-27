# ADR 0117: Native FAT32 check mode

## Status

Accepted. Completes GoOS 0.1.4 Phase 4. Repair remains Phase 5.

## Context

Mount-time validation protects the running filesystem but cannot prove global
cluster ownership, directory consistency or absence of lost allocation. The
checker must not race a writable mount and must not gain whole-disk authority.

## Decision

`fat32.Check` is a read-only whole-volume pass over a `fat32.Device`. It emits
typed, streamable issues and a summary. It checks accepted/recovered BPB and
backup state, FSInfo signatures and free count, FAT-copy agreement, bounded
cluster chains, loops and cross-links, chain length versus file size, lost
clusters, directory chains and dot entries, LFN sequence/checksum/UTF-16
validity, duplicate short aliases, volume-label attributes and empty-file
cluster ownership. An ownership bitmap costs one bit per addressable cluster;
directory work is iterative rather than recursive.

`/system/bin/fsck-fat --check auto` is a native command. Its immutable command
manifest requests `GrantBlockCheck`. PID 1 first asks VFS for a clean unmount;
the request flushes and fails while any open handle refers to the volume. Only
after success does PID 1 replace the filesystem endpoint in the block service
with a private channel for the checker. The command receives that bounded
partition slice read-only by construction: its device adapter implements no
writes. Closing the endpoint lets normal supervision restart the FAT32 service
and reattach the volume.

Human output contains one stable `error CODE cluster=N entry=N value=N` line
per issue and a final machine-readable count line. Any filesystem issue or I/O
failure exits non-zero. A clean volume exits zero. Check mode never repairs.

## Consequences

- The checker cannot run against a live writable mount or escape its selected
  partition.
- Mount failure and checker findings are distinguished by typed status/codes.
- The production checker replaces the older test-only fsck-style traversal;
  tests inject cross-links, lost clusters and malformed LFN chains directly.
- Repair decisions, write ordering and idempotence are deferred to Phase 5.
