# ADR 0023: Freestanding TinyGo PID 1

## Status

Accepted

## Decision

PID 1 is compiled from Go using a project-owned TinyGo target for the
`x86_64-unknown-none-elf` triple. It uses the System V ABI, disables TinyGo's
scheduler, links at the native userspace base, and enters through a small
assembly stub. Kernel calls use the existing `int 0x80` ABI.

The runtime initially used a 16 KiB stack; the exec control-plane extension
raises the shared userspace stack bound to 32 KiB. It retains a fixed 64 KiB zero-initialized arena
served by a non-freeing bounded allocator. Exhaustion is fatal. The build emits
a static ELF intermediate; a host tool copies validated `PT_LOAD` segments into
the versioned GoOS native image format. The kernel never parses ELF.

The existing bounded Manager now runs inside PID 1. It receives process-death
notifications, applies restart policy and rate limits, requests launches by
executable path, validates correlated results, and updates its own service
state. Kernel bootstrap policy simulations are removed.

## Consequences

Restart policy and service state no longer live in the kernel. The boot test
observes three independent replacement processes followed by the userspace
rate-limit marker.

The runtime is intentionally small: it has no goroutine scheduler, general
clock source, or reclaiming allocator. Spawn capability-manifest transport and
installation remain separate work.
