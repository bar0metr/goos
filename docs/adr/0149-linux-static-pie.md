# ADR 0149: Static position-independent Linux executables

## Status

Accepted. Extends ADR 0133.

## Context

ADR 0133 accepted `ET_EXEC` images only. Current toolchains link
position-independent executables by default, and `gcc -static-pie` produces a
static program as an `ET_DYN` file with no interpreter. The 0.2.0 parser
refused those.

It also refused every program-header kind it did not act on. Alpine's static
BusyBox is an `ET_EXEC` file with two `PT_NOTE` headers and a
`PT_GNU_PROPERTY` header, and the parser turned it away for them.

## Decision

**Static PIE.** The parser accepts `ET_DYN` when the caller configures a load
base. It adds the base to the entry point and to the address of every
`PT_LOAD`, `PT_PHDR`, `PT_TLS`, `PT_GNU_RELRO`, and `PT_DYNAMIC` header, and
runs each existing check on the moved address: user range, page congruence,
overlap, W^X, and the executable entry. An address that wraps is refused. The
mapper, the fault path, and `brk` work from the moved plan and know nothing of
the file type.

**One fixed base.** GoOS loads every such image at `0x40000000`. The lower
half of the 2 GiB image area stays with `ET_EXEC` programs and the upper half
holds the image and its heap. The base never varies. Randomizing it needs
entropy at every launch, which a CPU without a generator cannot supply
(ADR 0151), and a layout that differs between two machines would make the
Linux boot test depend on the CPU it runs on.

**No relocation in the kernel.** `PT_DYNAMIC` is accepted in an `ET_DYN` image
only, once, and only inside a load segment's file-backed bytes. The kernel
reads none of it. A static PIE relocates itself: musl's `_dlstart` and glibc's
`_dl_relocate_static_pie` find the load base from `AT_PHDR` and apply their
own `R_X86_64_RELATIVE` entries. `AT_PHDR` and `AT_ENTRY` therefore carry the
moved addresses.

**Interpreter.** `PT_INTERP` is refused in both file types. A dynamically
linked PIE names one and does not load.

**Descriptive headers.** `PT_NOTE`, `PT_GNU_EH_FRAME`, and `PT_GNU_PROPERTY`
describe an image to tools. The parser checks that their file range exists
and ignores them. Every other unknown kind is still refused.

## Consequences

A static-pie musl program built by a stock distribution compiler runs: a copy
of the conformance probe built with Alpine's `gcc -static-pie` passes its
hostile mode and the two-process check on GoOS. Alpine's static BusyBox loads.

The in-tree fixture `hello-pie` has no libc and no relocation. It compares
`AT_ENTRY` and the base it derives from `AT_PHDR` against the address it runs
at, and prints only when they agree.

A program that prints its own addresses prints the same ones on every run. An
image whose startup code does not relocate itself faults in its own code, as
it would on Linux without an interpreter. A shared object with an entry point
and no interpreter is a static PIE by this definition and loads.
