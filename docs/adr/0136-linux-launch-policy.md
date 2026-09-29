# ADR 0136: Linux launch policy

## Status

Accepted.

## Context

PID 1 already resolves executable paths, prepares stable executable sources,
and supplies explicit capabilities. Linux support must not create a second
launcher or grant device authority merely because an ELF header was found.

## Decision

PID 1 may classify `GOOSNAT` and ELF magic to choose a candidate launch format;
the kernel independently performs final validation. The common launch pipeline
retains executable-source lifetime, address-space creation, accounting,
scheduling, process handles, waits, and teardown.

A Linux launch receives only stdin, stdout, stderr, one fresh VFS session, a
resource profile, argv, environment, and cwd. It receives no loader, system
image, PID 1 launch, device, DMA, IRQ, port, framebuffer, raw-keyboard, or log
authority. Native services remain native-only in 0.2.0. Arbitrary ELF execution
is not an ambient permission: the normal executable-source Execute authority
still applies.

The kernel sets `LinuxX8664` only after Linux ELF validation succeeds. A failed
classification or validator never creates a Linux Process.

## Consequences

Shell and PID 1 stay the policy boundary, while the kernel remains the format
and isolation authority. Native launch manifests and `GST4` remain unchanged.

Lifecycle protocol version 10 carries the candidate format. PID 1 permits
direct children of `/system/linux` in addition to ordinary `/system/bin`
commands and gives every Linux launch exactly three stream endpoints and one
fresh VFS session under the `large` resource profile. The kernel reclassifies
the stable source, requires those four named grants with exact rights, builds
the System V stack, and retains cwd plus resolved handle roles in a bounded
compatibility table. Failed launches and process teardown remove that state.
