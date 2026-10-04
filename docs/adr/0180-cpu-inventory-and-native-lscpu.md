# ADR 0180: CPU inventory is normalized at boot and exposed read-only

## Status

Accepted.

## Context

CPU identity comes from CPUID, while the ACPI MADT describes the processors
firmware made visible. Neither source alone describes GoOS state. CPUID package
capacity is not a configured CPU count, and a MADT entry does not mean its
processor is scheduled. Raw CPUID, MSR, APIC, and firmware-table access would
also bypass the kernel's authority boundary.

The scheduler remains uniprocessor. It preserves x87 and SSE state with
FXSAVE, but does not enable or preserve AVX extended state.

## Decision

The amd64 architecture layer owns the raw CPUID instruction. Pure code in
`kernel/cpu/x86` parses bounded leaves for identity, topology, deterministic
caches, frequencies, virtualization, and feature bits. `kernel/acpi` validates
the RSDP, root table, and MADT checksums and lengths, accepts enabled Local APIC
and x2APIC processor entries, and rejects duplicates or inventories above 256
processors.

Boot combines both sources once. The bootstrap processor is dense logical CPU
0 and is online. Other enabled firmware processors are present and offline
until SMP exists. Missing or inconsistent firmware data falls back to one
present and online bootstrap processor. Topology is published only when every
present APIC ID can be decomposed consistently.

The kernel stores fixed records without pointers. Native calls 50–52 copy the
summary, one logical CPU, or one cache record to any registered process. They
require no capability and provide no operation that starts a processor,
changes scheduling, reads an MSR, controls an APIC, or submits arbitrary
CPUID leaves.

Hardware-advertised and user-usable feature bitmaps are separate. The usable
bitmap omits XSAVE, OSXSAVE, FSGSBASE, PKU, AVX-family instructions, GFNI,
VAES, and VPCLMULQDQ. The kernel does not enable their required CR4 or extended
state, and the scheduler does not preserve it. Missing facts are omitted rather
than estimated.

`/system/bin/lscpu` formats only these immutable records. It never accesses
firmware or hardware directly.

## Consequences

CPU reporting is bounded, deterministic, host-testable, and reusable by a
future SMP implementation. Present CPUs must not be confused with online CPUs.
The inventory is a boot snapshot and has no hotplug semantics. NUMA,
microcode, vulnerabilities, current frequency, BogoMIPS, and guessed topology
remain unreported.
