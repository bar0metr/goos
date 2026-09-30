# ADR 0139: Static musl fixture and startup calls

## Status

Accepted.

## Context

The release needs proof that a program built against an ordinary libc starts
through its normal entry path. The proof has to run offline, name its exact
inputs, and avoid kernel calls that report success for work they skip. The GoOS
build has no Linux cross-toolchain, and the canonical tests may not download
one.

## Decision

The fixture is a C program linked statically and non-PIE against musl. TinyGo,
which the GoOS build already requires, supplies the compiler, the linker, and
the musl sources. `userspace/linux/build.sh` compiles those sources and links
the fixture with no network access.

The repository commits the linked binary and its SHA-256. A host test compares
the two and feeds the binary to the kernel ELF parser. Building the boot image
runs no C compiler. Rebuilding the fixture is a deliberate step that updates
the binary, the checksum, and `userspace/linux/README.md` together, so a TinyGo
upgrade cannot change the fixture unnoticed.

musl startup and stdio need two calls beyond the memory and TLS set:

- `set_tid_address` stores the address and returns the thread ID. The ID is a
  per-process compatibility PID between 2 and 32767 and never a native process
  or task ID. Linux clears the stored address at thread exit only while another
  task shares the address space. A GoOS Linux process has one task, so exit
  writes nothing.
- `writev` carries all musl stdio output. It accepts at most 16 elements and
  moves at most 64 KiB per call. It validates every element before the first
  byte moves and returns a short count when the stream fills.

musl asks whether stdout is a terminal with `ioctl(TIOCGWINSZ)`. The call
returns `ENOSYS`. musl treats any failure as "no terminal" and buffers output,
so terminal ioctls stay unimplemented. The recorded trace contains no other
startup call: `rt_sigaction`, `rt_sigprocmask`, `set_robust_list`, and
`prlimit64` remain `ENOSYS`.

## Consequences

One pinned binary makes the libc proof repeatable on any host that can run the
boot test. The fixture's trace is the list of calls musl startup requires, and
each has real single-task semantics. A different libc, or musl with threads,
needs its own measured trace before the kernel gains calls for it.
