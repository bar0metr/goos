# ADR 0040: Capability-protected native byte streams

## Status

Accepted.

## Context

Explicit native standard-stream roles exist, but every role still names the
serial console device. Shell redirection and pipelines require an alternate
endpoint that preserves least privilege, bounded resource use, and peer-close
semantics without importing POSIX file descriptors into the native ABI.

Typed IPC messages are unsuitable as the direct byte-stream contract: their
request/reply correlations and message boundaries would leak into ordinary
utility I/O and complicate EOF and backpressure.

## Decision

Add a bounded unidirectional stream object with separate reader and writer
capability endpoints. Reader handles carry only `Read|Transfer`; writer handles
carry only `Write|Transfer`. The object contains a fixed 4096-byte ring buffer,
and each syscall moves at most 256 bytes through a validated 24-byte descriptor
that also returns the transferred byte count.

Empty reads and full writes report non-blocking backpressure. Closing the writer
allows buffered data to drain before reads observe peer closure as EOF. Closing
the reader discards buffered data and causes writes to report peer closure.
Normal handle close and process teardown both update endpoint state and release
fully closed objects.

Stream creation is available to ordinary processes because it grants no
authority beyond newly created private endpoints. Authority reaches another
process only through existing capability transfer and startup-manifest paths.

## Consequences

GoOS now has a native object suitable for pipes and redirected standard streams,
with bounded memory and enforced endpoint direction. The object is independent
of the serial console and VFS.

This iteration intentionally keeps operations non-blocking. Scheduler-backed
stream waits, startup-role substitution, and shell pipeline construction remain
required before exposing `|`, `<`, or `>` syntax.
