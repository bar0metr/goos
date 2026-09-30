# ADR 0148: Linux release fixtures, reference runs, and interoperability

## Status

Accepted. Extended by ADR 0152, which adds a strict reference run on a real
Linux kernel, and by ADR 0149, which adds a sixth fixture.

## Context

The release has to show four things with programs nobody converted to the
native format: a program with no libc runs, a static musl program runs, Linux
file calls reach the GoOS VFS, and a real utility suite works. It also has to
show that the results are right, which means the same as on Linux, and that a
Linux process gains no authority.

## Decision

**Fixtures.** The system image carries five prebuilt static programs under
`/system/linux`, each pinned by checksum and built by one offline recipe
(ADR 0139):

| Program | Shows |
| --- | --- |
| `hello` | assembly, `write` and `exit_group`, no libc |
| `musl-hello` | libc startup: arguments, environment, TLS, heap, stdio |
| `fsprobe PATH` | create, write, read, and stat a file through the VFS |
| `dirprobe DIR` | list a directory with `getdents64` |
| `abi-probe` | the conformance suite and the hardening modes |

**Reference.** `abi-probe` checks every result it gets and prints one line
per group. The same binary runs on a Linux kernel in a container
(`make test-linux-reference`) and must print the same lines; its recorded
call trace is in the repository. A GoOS result that differs from Linux by
design is checked only on GoOS, which the program recognizes from `uname`,
and each such difference is listed in the ABI document.

**Boot gate.** `make test-linux-boot` boots a validation image once. PID 1
runs the raw probes and `abi-probe` before the shell starts. The script then
runs from the shell: the four proofs on the volatile root and on FAT32, a
Linux program on each side of a native pipeline, two Linux processes at
once, the hostile, exhaustion, and isolation modes, an I/O port access that
must end the process, and Ctrl-C on a Linux process blocked on a stream, in
a sleep, and in a wait for the VFS. `make test-all` and the release gate
include it. The general boot test additionally restarts the VFS under a
Linux process and makes the VFS answer one late.

**BusyBox.** The repository carries no BusyBox. `make test-linux-busybox
BUSYBOX=path` places a static x86_64 BusyBox the caller supplies in the
image and requires six applets to work: `echo`, `pwd`, `cat`, `ls`, `mkdir`,
and `rm`. It then runs further applets and reports which of them run,
without requiring any, and prints the kernel's record of the call numbers it
refused. A call enters the kernel only when such a measurement or the
release scope asks for it, and only with real semantics; the others stay
`ENOSYS`.

**Timing.** `make test-linux-bench` times `getpid`, `clock_gettime`, a
stream write, and a file read beside native `bench` cases from the same
boot and writes a report. It is a baseline for comparing changes on one
host and no gate.

## Consequences

Linux behavior is a measured input instead of a recollection: a wrong
expectation in the probe fails on Linux before it misleads the kernel. The
reference runs under user-mode emulation on an arm64 host, which alters a
few results; the probe accepts both where that happens and says so. BusyBox
`sh` starts and runs a builtin, but it cannot run a program: `fork` and
`execve` are outside this release.
