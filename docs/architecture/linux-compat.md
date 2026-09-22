# Linux compatibility (planned)

**Status: not implemented.** No Linux ELF loading, Linux syscall entry, or
translation code exists in the tree. This document records the intended
design so that native work does not close off the path. It follows roadmap
Priority 5 and will be refined by an ADR before implementation starts.

## Principles

- Compatibility is a separate layer, planned under `compat/linux`. The native
  kernel ABI, handle model, and IPC protocols do not change to fit POSIX.
- Linux programs get no authority that a native program with the same
  manifest would not get. Translation must not bypass VFS, IPC, handles, or
  capabilities.
- Each translated syscall is bounded like a native call: validated lengths,
  bounded buffers, deadlines on blocking paths.
- Unsupported syscalls fail with `-ENOSYS` and are counted, rather than being
  approximated silently.

## Components

### ELF loader

Linux ELF is a distinct format from the native envelope
([native-images.md](native-images.md)). Planned stages:

1. validate the ELF64 header (x86_64, `ET_EXEC`, then `ET_DYN` for static PIE);
2. validate program headers: bounded count, page-aligned `PT_LOAD` segments in
   the user range, no overlap, no W+X, file ranges inside the image;
3. reject `PT_INTERP` until a dynamic loader exists;
4. map and zero-fill segments with the same rules as the native loader;
5. build the Linux initial stack: `argc`, `argv`, `envp`, and a minimal auxv
   (`AT_PAGESZ`, `AT_PHDR`, `AT_PHNUM`, `AT_ENTRY`, `AT_RANDOM`).

### Syscall entry

Linux programs use the `syscall` instruction with the Linux register
convention. That entry path is distinct from the native `int 0x80` gate, so
a process is either native or Linux and the kernel never guesses which ABI a
call uses.

### Translation

Syscalls map onto native primitives:

| Linux | Native mechanism |
| --- | --- |
| `read`, `write` on files | VFS session requests |
| `read`, `write` on stdio and pipes | stream endpoints |
| `openat`, `close`, `fstat`, `getdents64`, `lseek` | VFS session requests plus the fd table |
| `pipe2` | `StreamCreate` |
| `exit`, `exit_group` | `Exit` |
| `clock_gettime` | `ClockReadNanos` |
| `execve`, `wait4` | exec request to PID 1, process handle wait |
| `ppoll`, `poll` | `WaitAny` |
| `brk`, `mmap` (anonymous) | a bounded per-process region; no file mappings at first |

Whether translation runs in the kernel, in a per-process userspace shim, or
in a compatibility service is an open decision for the ADR. The constraint is
that authority comes only from the process's handle table.

### File descriptors

Each Linux process has a bounded fd table mapping small integers to native
handles plus per-fd state (VFS open-file ID, offset, flags). Descriptors 0, 1,
and 2 bind to the `in`, `stdout`, and `stderr` startup roles. `dup`, `dup2`,
and `fcntl` operate on this table; closing the last reference closes the
handle.

### Namespace view

Linux paths resolve through the process's VFS session. The expected layout
for early targets:

- `/persist`, `/tmp`, and `/system` as today;
- `/dev/null`, `/dev/tty` provided by the compatibility layer or a service;
- `/proc` and `/sys` absent until a program needs a specific file.

## Iterative targets

| Step | Target | Needs |
| --- | --- | --- |
| 1 | static x86_64 "hello world" | ELF loader, `write`, `exit` |
| 2 | minimal syscall translation | fd table, file I/O through VFS |
| 3 | static musl program | `brk`/`mmap`, `arch_prctl`, TLS, auxv |
| 4 | BusyBox | `execve`, `wait4`, pipes, directories, environment |
| 5 | termios, signals, polling | TTY service, signal delivery, `poll` |
| 6 | `nano` | all of the above, terminal control |

Each step is gated on host tests and a QEMU check, like native features.
Dynamic linking, glibc, threads, networking, and broad ABI coverage are not
planned for these steps.
