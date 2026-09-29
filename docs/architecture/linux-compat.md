# Linux compatibility

**Status: GoOS 0.2.0 foundations in progress.** Phase 0 decisions are recorded
in ADRs 0132 through 0137. Phase 1 implements immutable Process ABI identity.
Phase 2 implements the dedicated Linux x86_64 `SYSCALL`/IRETQ entry and rejects
wrong-ABI entry at both gates. Linux ELF loading and semantic syscall
translation are not implemented yet.

## Principles

- Compatibility is a separate kernel subsystem. The native
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
([native-images.md](../kernel/native-images.md)). Planned stages:

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
call uses. Process identity is kernel-owned and immutable. The native gate
authorizes only `Native`; the MSR-backed Linux gate authorizes only
`LinuxX8664` (ADR 0132).

The Linux gate switches from the untrusted user RSP to the current task's
guarded kernel-entry stack through supervisor-only GS data before using the
stack or entering Go. It then installs the kernel CR3, snapshots the Linux
register ABI into a fixed frame, and returns through a higher-half IRETQ
prefix. Unknown syscalls currently return `-ENOSYS`.

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

Translation runs in a separate kernel compatibility subsystem for 0.2.0. It
may access caller memory after the ordinary address-space checks, but authority
still comes only from process-owned resources. It cannot call filesystem or
device implementations directly (ADR 0134).

### File descriptors

Each Linux process has a bounded fd table mapping small integers to private
compatibility entries plus per-fd state. An fd is never a native handle.
Descriptors 0, 1, and 2 bind to the supplied `in`, `stdout`, and `stderr`
streams. Files are reached only through the process's explicit VFS session
(ADR 0135).

### Namespace view

Linux paths resolve through the process's VFS session. The expected layout
for early targets:

- `/persist`, `/tmp`, and `/system` as today;
- `/dev/null`, `/dev/tty` provided by the compatibility layer or a service;
- `/proc` and `/sys` absent until a program needs a specific file.

## Launch and security boundary

PID 1 may classify native magic versus ELF magic, but the kernel independently
validates the selected format. A Linux launch receives only supplied standard
streams, one fresh VFS session, argv, environment, cwd, and a resource profile.
It receives no device, loader, system-image, PID 1 launch, DMA, IRQ, port,
framebuffer, raw-keyboard, or kernel-log authority (ADR 0136).

The common launch path retains executable-source lifetime, address-space
creation, accounting, task creation, waits, and teardown. Format-specific code
owns validation, mappings, entry state, and startup layout. Linux programs do
not receive the native `GST4` startup object (ADR 0133).

FS base will be task context and will be saved and restored on every context
switch. Only `ARCH_SET_FS` is planned for 0.2.0; `ARCH_SET_GS` is rejected and
kernel GS remains architecture-owned (ADR 0137).

## Iterative targets

| Step | Target | Needs |
| --- | --- | --- |
| 1 | static x86_64 "hello world" | ELF loader, `write`, `exit` |
| 2 | static musl program | `brk`/`mmap`, `arch_prctl`, TLS, auxv |
| 3 | VFS probes | fd table, file I/O and directories through VFS |
| 4 | selected BusyBox applets | measured P1 syscalls; no shell |

Each step is gated on host tests and a QEMU check, like native features.
Dynamic linking, glibc, `fork`/`clone`, Linux `execve`, threads, full signals,
networking, BusyBox shell, and `nano` are outside GoOS 0.2.0.
