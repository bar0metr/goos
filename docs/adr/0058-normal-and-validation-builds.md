# ADR 0058: Normal and validation builds

## Status

Accepted. Implements roadmap Priority 4.

## Context

Every boot ran the kernel's self-tests. The boot sequence included:

- a scheduling demonstration;
- two fixed processes that probe address-space isolation, IPC, TTY rights,
  and fault containment, one of which faults deliberately;
- in-kernel checks of RAMFS, VFS, bounded channels, and shared memory;
- a supervision scenario in PID 1 that crashes a probe service three times;
- a block-service probe that writes test bytes to the persistent disk.

Every stage and utility printed `*_OK` markers for the boot test, so `make run`
produced about a hundred lines before the prompt, and each utility printed a
marker after its output. The self-test code was interleaved with the setup of
PID 1 in one 670-line `main`.

## Decision

A `goos_validation` build tag, selected with `VALIDATION=1`, sets the constant
`validation.Enabled` in `internal/validation`.

In a normal build:

- the kernel brings up the hardware and prints a short banner: release, CPU,
  memory, timer rate, security features, and scheduler;
- it creates PID 1 with the same fixed capability layout as before, and prints
  `Kernel initialized`;
- PID 1 prints `Starting services...` and registers and launches the services
  directly, printing `[ OK ] name` or `[FAIL] name` for each;
- the shell prints a blank line and the prompt;
- the demonstration kernel tasks sleep permanently at once;
- no marker is printed and no fault is injected.

A validation build does everything the previous boot did. The kernel's
self-tests move to `kernel/selftest.go` behind the tag. They are the scheduler
demonstration, processes 2 and 3, the IPC, RAMFS, VFS, bounded-channel, and
shared-memory checks, and the post-boot checks. PID 1 runs the supervision
scenario and the storage probe before starting the same services. Markers are
printed through `mark` helpers that compile to nothing in a normal build.

`make test-boot` always builds the validation ISO. The stress and latency
harnesses wait for the first prompt instead of a marker and run the normal
build, so they exercise the system as shipped. `internal/version` names the
release reported by the kernel banner and `uname`.

## Alternatives

Filtering markers on the host would keep the self-tests, and the disk write,
in every boot. A runtime switch would ship the test code in every image and
need a way to set it before PID 1 starts.

## Consequences

A normal boot is quiet, does not write the test pattern to the persistent disk,
and does not spend time on fault injection or supervision restarts. Process IDs
2 and 3 exist only in validation builds; services still start at PID 4.

Errors are still printed as bare diagnostic codes. ADR 0059 turns them into
structured log events.
