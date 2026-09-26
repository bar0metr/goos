# Coding style

These rules describe how the existing code is written. Follow them for new
code; propose changes through an ADR.

## Packages

- Small packages with one responsibility: `kernel/handle`, `kernel/ipc`,
  `kernel/stream`, `kernel/wait`, `userspace/startup`, `userspace/block`.
- Keep policy and protocol cores free of hardware and runtime dependencies so
  they build and test on the host. Hardware glue lives in files with the
  `uefi` build tag (`*_uefi.go`) or in `kernel/arch/amd64`.
- A userspace program is a thin `userspace/cmd/<name>/main.go` over a
  host-tested package in `userspace/<name>`.
- Wire formats are encoded and decoded explicitly, little-endian, with a
  protocol identifier and version. Go struct layout is never an ABI.

## Bounded by construction

- Use named capacity constants (`handle.MaxHandles`, `ipc.QueueCapacity`,
  `kcall.MaxStreamIO`) chosen as system limits, not prototype conveniences. A
  registry of large entries holds pointers and takes each entry's pages when
  it is created (`objects.Storage`, `vm.NewObject`; ADR 0111). No kernel
  structure grows past its limit.
- Range over a table's slice (`table[:]`), not the array: ranging over an
  array value copies it onto the kernel stack.
- Every loop over untrusted data has a bound independent of that data.
- Every blocking operation has a deadline, or is explicitly infinite and
  documented as such.
- Check lengths before slicing and check `a + b` for wraparound before using
  it as an address, offset, or deadline.
- Kernel hot paths use static buffers, not allocation.

## Errors

- Return typed statuses (`kcall.Status`, `ipc.Status`, `vfs.Status`,
  `stream.Status`, `blockproto.Status`) or `(value, ok bool)`. Do not return
  strings or wrap errors.
- Map internal statuses to `kcall.Status` at the native-call boundary.
- The kernel does not panic on untrusted input. It halts only for boot
  failures and broken internal invariants, with a `PANIC:` line first.
- Services answer malformed but attributable requests with an error reply
  instead of dropping them (ADR 0048).
- On a multi-step operation, roll back partial state on failure (see
  `ChannelCreate` in `kernel/ipccalls.go` and manifest installation in
  `kernel/launch.go`).

## Constants

No magic numbers in logic. Name protocol sizes, limits, and bit masks.
Existing exceptions (hard-coded boot handle IDs, the virtio status port
offset used in teardown) are debt, not precedent.

## Comments

- Few comments. Names and types should carry the meaning.
- Comment *why*: an invariant, an ordering requirement, a race being closed,
  a reason a simpler approach is wrong. See the register-then-recheck comments
  in the wait paths of `kernel/waits.go`.
- Doc comments on exported identifiers whose contract is not obvious.
- Every `unsafe` use in the kernel needs a `// SAFETY:` comment stating the
  ownership and bounds invariant ([memory-safety.md](../security/memory-safety.md)).

## `unsafe`

Allowed in the kernel only under `uefi`, `vm`, and `amd64` directories, and in
userspace only under `userspace/runtime/` (`sys` for native calls and mapped
windows, `arena` for the allocator). `make lint` enforces both, including
`//go:linkname` and `//go:extern` in userspace (ADR 0095). A program that needs
a native call the wrappers lack adds a typed wrapper to `userspace/runtime/sys`
rather than a stub of its own.

## Formatting and checks

- `gofmt`; `make fmt` rewrites, `make lint` fails on unformatted files.
- `go vet ./...` through `make lint`.
- `make test` before every commit; `make test-all` before delivering kernel,
  userspace, protocol, storage, or boot changes ([testing.md](testing.md)).
- Security-relevant packages belong in the `test-security` list in the
  `Makefile`.

## Decisions and documentation

- Architectural decisions get an ADR in `docs/adr/`, numbered sequentially:
  Status, Context, Decision, Alternatives (when relevant), Consequences.
  Superseding or correcting an ADR is a new ADR that references the old one.
- Update the matching `docs/architecture/*.md` in the same change.
- New documents are added to the `docs` target in the `Makefile`.

## Commits

One short subject line, imperative, with a conventional prefix such as
`feat:`, `fix:`, `perf:`, `docs:`. Put rationale in the ADR, not in a long
commit body.
