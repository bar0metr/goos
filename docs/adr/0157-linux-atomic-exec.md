# ADR 0157: Linux exec replaces a prepared process image atomically

## Status

Accepted.

## Context

Linux `execve` keeps process identity and inherited resources while replacing
every byte of process-private VM state. Destroying the old address space before
the candidate is known to be usable would leave no program to receive an ELF,
pointer, or resource error.

The VFS session is the process's filesystem authority. The kernel cannot reopen
an executable through a separate privileged path, and a mutable pathname is not
a stable source for demand paging after the call returns.

## Decision

The kernel copies the path, argument vector, and environment vector into a
bounded kernel snapshot before blocking. It accepts at most 256 arguments, 256
environment entries, 32 KiB per string, and 64 KiB of string material including
terminators. It checks every vector slot and string through the old address
space. An unreadable pointer is `EFAULT`; a missing terminator or exceeded bound
is `E2BIG`.

The VFS pins the candidate and fills a private executable object with the whole
file. The kernel accepts the same static x86_64 ELF classes as initial launch:
`ET_EXEC` and supported static `ET_DYN`, with no interpreter. A native GoOS
image, dynamic ELF, malformed ELF, or invalid layout is `ENOEXEC`.

ELF parsing, region planning, page-table creation, guarded kernel-stack mapping,
initial-stack construction, and page charging complete against a new root while
the old root remains active. The task root, compatibility root, and Process root
change only after preparation succeeds. The return path enters the new ELF entry
with its new RSP and clears inherited general registers. The old root is then
destroyed.

PID, GoOS Process identity, parentage, family, cwd, resource profile, native
process handle, and non-`CLOEXEC` descriptions survive. The new image starts
with a fresh brk range, no anonymous mappings, and FS base zero. Caught signal
dispositions reset to default, ignored dispositions and the blocked mask remain,
and no pending signal survives because this compatibility layer does not yet
deliver or queue signals.

Final VFS descriptions selected by `CLOEXEC` are closed through the shared
family context after VM preparation and before the root switch. A close failure
does not cancel the prepared replacement. Local `CLOEXEC` entries are removed at
commit; other entries retain their description and position.

The display name becomes the basename of the requested path. `AT_EXECFN` keeps
the requested path, and the initial stack is built only from the kernel snapshot.

## Consequences

Every reported pre-commit error returns to an intact old image. Preparation may
temporarily charge both address spaces and both executable objects to the
process, so a replacement can fail with `ENOMEM` even when either image fits by
itself. Executable bytes are copied once at exec time instead of being served by
the old pathname later.

The implementation remains single-task. It supplies no interpreter, shebang
handling, `execveat`, PATH search, or native-image transition.
At most four replacements may be in VFS preparation at once; a fifth receives
`ENOMEM` without changing its image.
