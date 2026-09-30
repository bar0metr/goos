# ADR 0152: Strict reference run on a Linux kernel

## Status

Accepted. Extends ADR 0148.

## Context

The reference run of ADR 0148 starts the conformance probe in an amd64
container. On an arm64 host the container runs under user-mode emulation, and
the emulator answers some calls itself. It checks a whole buffer before a
transfer and reports `EFAULT` where the kernel reports the bytes it moved. It
sleeps on a bad `nanosleep` pointer. It walks a range of the largest possible
length before it answers. It keeps its own signal table, which leaves
`SIGKILL` and `SIGSTOP` in a handler's mask, and it refuses a null buffer that
`/dev/null` never reads. The probe accepted both answers in those places, so
GoOS was held to a reference that was not Linux there.

## Decision

A second reference run boots a real Linux kernel in `qemu-system-x86_64`,
with nothing between the probe and the kernel:

```sh
make test-linux-kernel-reference KERNEL=/path/to/vmlinuz BUSYBOX=/path/to/busybox
```

`userspace/linux/run-on-linux-kernel.sh` packs an initramfs with the probe
and a static BusyBox as `/init`, boots the kernel the caller supplies with the
serial port as console, and runs the default and the hostile mode. The
repository carries neither the kernel nor BusyBox.

The run sets `ABI_PROBE_STRICT`. A strict probe accepts only the exact Linux
result in every check, and GoOS is always held to strict. The container run
stays as the quick check and as the source of the recorded call trace, with
the lenient checks the emulator needs.

The Linux boot test also gains a stack overflow: the probe recurses until it
reaches the guard below its 8 MiB stack, the kernel must log a guard-page
fault, and the shell must go on.

The timing target takes the accelerator from `QEMU_ACCEL`. An x86_64 host can
time the guest on its own CPU; an arm64 host can only translate it.

## Consequences

The first strict run found two GoOS answers that differed from Linux 6.12: a
negative `pread64` offset was checked after the descriptor, and a transfer
with a length that leaves the user half returned a short count. Both are
fixed (ADR 0150). The default and hostile modes now pass on Linux 6.12 in
strict mode and on GoOS.

The strict run needs a kernel image from outside the repository, so it is no
part of `make test-all`. Timings on the development host still describe
translated code.
