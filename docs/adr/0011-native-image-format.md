# ADR 0011: Versioned native image format

Status: Accepted

## Context

Bootstrap programs were byte arrays manually copied and mapped by kernel main.
That bypassed a loader boundary, mixed process policy with VM mechanics, and
provided no format on which initramfs process creation could build.

## Decision

Introduce the versioned `GOOSNAT\0` native envelope with an explicit entry,
stack pointer, and bounded segment table. Parse it without depending on Go
structure layout. Validate all offsets, sizes, addresses, permissions, reserved
fields, entry placement, stack placement, and mapping conflicts before exposing
payload pages. Limit the bootstrap implementation to eight one-page segments.

Load every payload into a zeroed physical page before installing final user
permissions. Keep Linux ELF parsing outside this native loader.

## Alternatives

Raw machine-code blobs cannot describe permissions or reject malformed layout.
Using Go structure memory as the format creates an unstable compiler-dependent
ABI. Adopting Linux ELF for native programs would make the compatibility ABI a
foundation of the native system.

## Consequences

Both live boot processes now use one validated loading path, and malformed
images have host-side negative tests. The current envelope is intentionally
minimal. Multi-page segments, relocations, ASLR, initramfs lookup, page-table
branch allocation, and complete rollback require later extensions.
