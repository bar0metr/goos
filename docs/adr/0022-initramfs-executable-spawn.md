# ADR 0022: Initramfs executable spawn

## Status

Accepted. The kernel-side path lookup is superseded by ADR 0093: launches name
an extent of the system image, and only the bootstrap `system/init` is found by
name.

## Decision

The PID 1 lifecycle protocol carries a bounded normalized absolute executable
path in each launch request. The kernel resolves it relative to the immutable
initramfs root. Relative paths, empty components, NUL bytes, and parent
traversal are rejected before the request is accepted.

An authorized request is resolved from the boot archive and passed through the
native image validator. The kernel then creates a private address space,
assigns a fresh process identifier, stores the Process in a fixed-capacity pool,
loads the image, creates its initial Task, and returns a correlated typed launch
result. The backend contains no service-name or executable-specific branch.

## Consequences

Service selection is no longer hard-coded in the kernel and executable images
are packaged as independent initramfs entries. The lifecycle channel remains
the authority to request creation; arbitrary processes cannot invoke this
control plane.

This does not move restart policy into userspace. PID 1 is still a generated
bootstrap native program because TinyGo 0.42 has no ready freestanding amd64
userspace target. A dedicated runtime and image-linking pipeline are required
before the Go supervisor can execute at ring 3.
