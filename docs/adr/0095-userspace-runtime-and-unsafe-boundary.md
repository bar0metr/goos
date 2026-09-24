# ADR 0095: The userspace runtime and its unsafe boundary

## Status

Accepted.

## Context

Every userspace program carried its own copy of the same low-level code: the
bump allocator and TinyGo runtime hooks over `_heap_start`/`_heap_end`, a block
of `//go:linkname` declarations for the native-call stubs in
`userspace/runtime/amd64.S`, and the `unsafe.Pointer` conversions that build
native-call descriptors. The shell, PID 1, VFS, the terminal, the log and
configuration services, the block service, the storage driver, and the command
library each had them; PID 1 alone declared 23 stubs. The allocators differed
only in the name of the out-of-memory marker.

`make lint` checked `unsafe` only in the kernel. In userspace nothing stopped
the next program from adding raw pointer code wherever it was convenient, and
the audit surface was every service's `main.go`.

## Decision

**Two packages under `userspace/runtime/` hold all userspace `unsafe`.**

- `userspace/runtime/sys` declares every native-call stub once, unexported, and
  wraps each in a typed function: handles are `handle.ID`, buffers are slices,
  text is a string, results are `kcall.Status` plus values
  (`sys.StreamRead(stream, buffer) (int, kcall.Status)`,
  `sys.ProcessWait(process, timeout) (code, status)`). Descriptors the kernel
  reads are built inside the wrapper from package scratch variables, so the
  wrappers do not allocate: every userspace process runs one task, so no two
  calls are ever in flight. The package also owns the few places userspace turns
  an address into memory: the system image (`ImageMap`), shared pages
  (`MemoryMap`), DMA regions (`DMAAllocate`), and the launch staging page the
  kernel maps into PID 1 before it starts (`MappedAtStart`); and the zero-copy
  `String` and `Bytes` views.
- `userspace/runtime/arena` is the allocator: `Init`, `Current`, `Reset`, an
  `OnExhausted` hook for the program's out-of-memory report, and the TinyGo
  runtime hooks. Allocation still zeroes (ADR 0048).

**Everything else is plain Go.** Programs, services, commands, and libraries
(`userspace/log`, `userspace/wait`, `userspace/utility`) call the wrappers.
`userspace/wait.Interest` is an alias of `sys.Interest`, whose layout is the
kernel's wire record, so the wait API did not change.

**Lint enforces it.** `tests/lint/unsafe_boundary.sh` refuses `"unsafe"`,
`//go:linkname`, and `//go:extern` under `userspace/` outside
`userspace/runtime/`, alongside the kernel rule it replaces. It runs its own
self-test first, so a broken check cannot pass silently.

Wire formats, protocol versions, and native-call numbers are unchanged; this
moves code, it does not change behavior.

## Alternatives

**One package per concern (`abi`, `ipc`, `memory`).** The native calls, the
descriptors, and the mapped windows are one ABI and share the scratch state;
splitting them would add exported plumbing between packages whose only client
is each other.

**A generic IPC client layer.** Several services repeat send, wait-receive, and
decode loops with different deadlines and error reactions. Folding them into
one helper would hide those differences, which are policy; only the raw
transfer moved.

**Keeping `unsafe` in `cmd/` and adding SAFETY comments.** Cheaper, but the
audit surface stays at every service, and the next service copies the pattern
again.

## Consequences

`unsafe` in userspace is two small packages with SAFETY comments. A service no
longer declares stubs or an allocator, which is recorded in
`docs/development/adding-service.md`.

One diagnostic lost detail: VFS's session-corruption dump no longer prints the
address of its wait array, which only a raw pointer conversion could produce.
The kernel's `Diagnostics` trace of the session table, which it still passes,
covers the same question.

Image sizes at the default `-opt=2` moved both ways, by the compiler's
inlining rather than by new code: the shell is 16 KiB smaller, the terminal
17 KiB and VFS 10 KiB larger, the commands within a kilobyte either way; about
9 KiB more across the whole system image.

`sys` compiles on the host so that libraries built on it keep their host
tests; there, `window` returns nothing, because no native call succeeds on the
host.
