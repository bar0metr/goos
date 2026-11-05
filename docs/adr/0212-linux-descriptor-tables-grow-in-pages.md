# ADR 0212: Linux descriptor tables grow in pages

## Status

Accepted. Amends ADR 0145 (`RLIMIT_NOFILE`) and builds on ADR 0111 and
ADR 0065.

## Context

Each Linux process had a fixed `[64]FDEntry` table inside its compatibility
state, and `RLIMIT_NOFILE` reported 64. Servers, shells running pipelines,
and test suites routinely hold more than 64 descriptors; a Linux login gives
them 1024 soft and 4096 hard. The kernel heap never frees (ADR 0065), so a
table cannot simply allocate, and a static 4096-entry table per process slot
would cost 1 MiB whether used or not.

Three things followed from the fixed size. The `Released` collection that
carries last references out of the tables held 65 descriptions and dropped
the rest. `select` kept six whole fd_sets and `poll` a 64-entry array in
per-processor scratch, both sized by the table. Exec found the VFS files to
close with a quadratic scan over the table.

## Decision

**A table keeps 64 entries inline and takes pages beyond them.** `FDTable`
holds descriptors 0 to 63 in itself. A descriptor past what the table holds
takes one page of 1024 four-byte entries from `objects.Storage[FDChunk]`,
which the kernel backs with `vm.NewObject` pages (ADR 0111) and host tests
with the Go heap. A table holds at most four pages, enough for
`LinuxFDCeiling` (4096). A process with a few descriptors costs no pages; one
at the default limit costs one. Pages stay until the process exits or is
removed, as Linux keeps a grown table.

**The owner pays.** Each page charges one `ResourcePages` to the native
process that owns the table. The launch path names it with `SetFDOwner`
after `InstallProcess`; `ForkProcess` takes the child's process and charges
the copy to it. The table keeps the owner's ID and charges or releases
nothing once the slot holds another process. A refused charge or a missing
page is `ENOMEM`, as a failed `expand_files` is on Linux, and a fork whose
child cannot pay for its copy fails whole.

**Limits are per process.** A table starts at 1024 soft and 4096 hard.
`prlimit64` changes `RLIMIT_NOFILE` as an unprivileged Linux process may: a
soft limit above the hard one is `EINVAL`, and a hard limit above the current
one or above 4096 is `EPERM`. Lowering the soft limit leaves open descriptors
open. Fork copies the limits and exec keeps them. Every other resource still
refuses a new limit with `EPERM`.

| Call | At or past the soft limit | Table cannot grow |
| --- | --- | --- |
| `open`, `socket`, `pipe`, `accept`, `eventfd`, `dup` | `EMFILE` | `ENOMEM` |
| `fcntl(F_DUPFD)` | `EINVAL` for the minimum, `EMFILE` when none is free | `ENOMEM` |
| `dup2`, `dup3` | `EBADF` for the target | `ENOMEM` |
| `poll` | `EINVAL` when `nfds` exceeds it | |

**Allocation stays lowest first.** The table keeps the lowest descriptor
below which all are open and searches from there, so a closed descriptor is
the next one handed out, however high.

**Released descriptions stay in their slots.** A last reference no longer
copies the description out. Its slot stays taken, marked parked, and joins a
list the `Released` holds through the slot's `next` field. `At` reads the
slot, and `Reset` frees every parked slot under the tables lock. A `Released`
costs the same whether it collects three descriptions or 4096. Callers
already reset after releasing; the race test now does too.

**The open-description pool keeps its size.** `MaxOpenDescriptions` was
`MaxProcesses * 64`; it is now 4096 by name, the system's file-max, and a
full pool is `ENFILE`. Other tables bound real use first: 64 VFS files per
session, 32 sockets per family, 256 streams, and dup shares a description.

**`select` and `poll` scan in windows.** `select` reads and writes the three
fd_sets 512 descriptors at a time and cuts `nfds` to the table's capacity, as
Linux cuts it to `max_fds`. It scans three times: once for `EBADF`, once for
readiness and native interests, and once more to write the sets when it
returns. `poll` reads 64 `pollfd` entries at a time, evaluates them once, and
evaluates and writes them again when it returns. The scratch is 192 bytes of
set windows and 512 bytes of `pollfd`, whatever the limit. The native wait
holds `wait.MaxObjects` interests; a scan that fills it bounds its sleep to
one tick so the descriptors left out are scanned again.

**Exec finds VFS files in two passes.** The first counts each description's
close-on-exec descriptors in its slot; the second takes a VFS file whose
every reference is marked and clears the count. The kernel keeps room for
`vfs.MaxOpenPerOwner` handles to close.

## Alternatives

- **A larger static table.** 4096 entries in each of 64 process slots is
  1 MiB of kernel memory for processes that mostly hold three descriptors.
- **Pages from the first descriptor.** A page per process for the common
  case of a dozen descriptors; the inline 64 cost 256 bytes.
- **Copy-out `Released` in pages.** Collecting into pages taken at release
  could fail at exit, when nothing can be refused.
- **Process-wide select buffers sized by the hard limit.** Six 512-byte sets
  per processor would work today and break again at the next limit.

## Consequences

- A Linux process can hold 1024 descriptors by default and 4096 after
  raising its soft limit; `abi-probe exhaust` lowers the soft limit to 64 to
  keep its exhaustion run short, and checks `dup2` to 1000 after raising it
  again.
- The kernel build allocates on the heap at the same places as before and no
  stack frame grew; the largest stays `compat/linux.route`.
- `select` and `poll` read user memory again on the pass that writes back. A
  second thread that changes the sets between the passes sees the result of
  the second pass, which is also the count returned.
