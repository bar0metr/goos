# Testing

GoOS keeps fast host validation separate from the complete QEMU gate.

## Targets

- `make test-unit` runs all host-side Go tests.
- `make test-integration` builds and validates the UEFI kernel artifact.
- `make test-security` runs the capability, handle, IPC, VM, filesystem, and
  userspace security-focused host tests explicitly.
- `make test` runs the three fast targets above and does not start QEMU.
- `make test-boot` builds the validation ISO (`VALIDATION=1`, ADR 0058) and
  runs the two-phase QEMU boot and reboot persistence test.
- `make test-all` is the canonical complete Definition-of-Done gate. It runs
  the fast tests, lint, documentation checks, the ISO build, and the real QEMU
  boot/persistence test in sequence so host builds cannot contend with QEMU.
- `make test-stress` boots `STRESS_RUNS` guests (default 4) in parallel
  batches of `STRESS_JOBS` and drives `STRESS_ROUNDS` rounds of commands
  through each; `make test-all` includes two runs.
- `make test-latency` boots one guest and measures command and VFS latency
  into `build/reports/latency-hz<rate>.md`. It is a measurement, not a gate,
  and is not part of `make test-all`.
- `make test-performance` writes microbenchmark reports under `build/reports/`.

Use `make test` during the normal edit loop. Use `make test-all` before delivery
of changes that affect the kernel, native userspace, startup protocols, storage,
shell composition, or boot behavior.

## QEMU boot test

`tests/boot/qemu_boot.sh` captures the complete serial streams in:

- `build/logs/boot.log` for the main validation boot;
- `build/logs/reboot.log` for the persistence boot.

The target creates a fresh `build/boot-test-virtio-blk.img` for each run. Tests
therefore cannot corrupt or overwrite the persistent `build/virtio-blk.img`
used by `make run`; both QEMU phases share the fresh test image to validate
reboot persistence.

The main phase has a 180-second bound. It validates kernel initialization,
memory isolation, scheduler/process behavior, IPC and capabilities, userspace
services, storage, filesystems, shell commands, pipelines, failure recovery,
resource-reuse stress, redirection, background jobs, and the on-disk persistent
bytes. It runs two background jobs while a foreground command and `services`
are served, lists them with `jobs`, stops one with `kill` and the foreground
command with Ctrl-C, and runs a four-stage pipeline whose request is larger
than one IPC message. The
reboot phase has a 60-second bound and reads the persisted file in a fresh VM
without recreating it. A third phase runs the M7 acceptance inside one VM,
logged to `build/logs/m7-reboot.log`. It creates `/home/m7`, writes a file in
it, runs the shell's `reboot`, and requires both boots and both reads of the
file. It uses `/home` because that is where the specification puts user data.

`make test-latency` runs with the log service present. A drain pass that has
records to write contends with the benchmark for the single-threaded filesystem
service, so the in-process `bench vfs` figure varies with whether one landed in
its window; the command latency medians do not move. ADR 0073 records the
isolation measurement.

The log group asserts that the log service started, that `/state` holds both the
live file and one rotation, and that a formatted record reads back out of the
file. The reboot phase then reads the rotated file, which can only have been
written before that boot, so a record surviving the reset is a persistence
proof. The validation build uses a smaller rotation bound so one session crosses
it.

The config group asserts that a value is checked against the type its key
declares, that a published value cannot be rewritten, that an undeclared key is
not a new setting, that the exported file holds the writable entries and not the
published ones, and that a value set in one session is restored in the next.

The release-shell group asserts the resource accounting: that `ps` reports
non-zero pages for PID 1, that `quota` shows an ordinary process holding a share
of the stream table rather than all of it, and that PID 1 holds the system
share. The `QUOTA_OK` marker comes from a validation-build check that drives the
same charge-then-create order `StreamCreate` uses and requires the refusal to
arrive while registry slots remain.

The filesystem group also asserts the namespace layout: that `ls /` names every
mount, that `/system` is the boot image with its twenty utilities listed under
`/system/bin`, that `mkdir /system/backdoor` is refused with "read-only
filesystem", and that a file written through `/home` reads back through
`/persist/home` as the same bytes.

The main phase also reads `runtime.heapptr` through the QEMU monitor at the
first prompt and again at the end, and fails if it moved: the kernel leaks every
allocation, so no repeating path may allocate (ADR 0065). The check is skipped
with a `SKIP` line where the monitor or `nc -U` is unavailable.

Successful runs print one `PASS` line per architectural group and a final check
count. Repeated guest output remains only in the full serial log. Failed checks
print a stable name, expected and observed values, the log path, and a bounded
context excerpt. Timeouts identify the phase and completion marker. The harness
never prints an entire serial log automatically.

Inspect the raw artifacts directly when more context is required:

```sh
less build/logs/boot.log
less build/logs/reboot.log
```

The diagnostic reporting path has a deterministic self-test that intentionally
returns failure without starting QEMU:

```sh
tests/boot/qemu_boot.sh --self-test-failure
```

## Stress test

`tests/boot/stress.sh` drives one guest through a FIFO instead of a timed
script: it sends a command, waits for the next prompt, and fails if the prompt
does not appear within `COMMAND_TIMEOUT` seconds (default 30) or if any fatal
service marker appears. Each round reads and writes the FAT32 mount, runs a
pipeline, launches a failing pipeline stage, creates and removes files, and
runs a command sequence, so every round exercises VFS session creation and
teardown, peer death, process-slot reuse, and PID 1 exec handling. Output is
checked for the round's persistent read-back and pipeline data.

`tests/boot/stress_matrix.sh` runs several guests in parallel, each with its
own disk under `build/stress/hz<rate>/`, and prints one line per guest. Run it
at every supported rate before changing scheduling or timer behavior:

```sh
make test-stress HZ=100 STRESS_RUNS=8
make test-stress HZ=250 STRESS_RUNS=8
make test-stress HZ=1000 STRESS_RUNS=8
```

On failure the driver sends `diag`, so a `DIAG=1` build leaves the kernel state
dump in that guest's log.

## Latency measurement

The kernel calibrates the TSC against the PIT at boot and serves nanoseconds
since boot through native operation 24 (ADR 0054). Two guest tools use it:

- `time <command line>` is a shell builtin. It runs the rest of the line
  through the normal path, so builtins, pipelines, redirections, and `;`
  sequences all work, and then prints `time: <N> us`. The time covers the
  shell's exec request, PID 1, every launch, and the wait for exit.
- `bench vfs N` sends N `Stat /` requests and `bench read PATH N` opens, reads
  to EOF, and closes PATH N times, both from inside one process with its own
  VFS session. Each prints `bench <mode>: N ops, total <us> us, avg <ns> ns/op`.
  N is at most 10000. A file under `/persist` exercises VFS, FAT32, the block
  service, and the virtio-blk driver, subject to the FAT32 sector cache.

`tests/boot/latency.sh` drives one guest through a FIFO the way the stress test
does. After a warm-up pass it runs each case under `time` `LATENCY_REPEATS`
times (default 10) and writes the median, p90 (nearest rank), and minimum per
case, followed by the `bench` lines:

```sh
make test-latency
make HZ=1000 test-latency LATENCY_REPEATS=20
make HZ=1000 DIAG=1 test-latency
```

The `help` case launches nothing and shows the measurement floor. The serial
transcript is kept in `build/logs/latency-hz<rate>.log`. In a `DIAG=1` build
the script ends with
`diag` and adds the `DIAG clock` and `DIAG launch` lines to the report: per
stage of `spawnExecutable` (image lookup, address-space creation, image load,
process and grant setup, task install), the number of launches since boot and
their total and average time. Failed lookups are counted too. Numbers from
QEMU TCG describe the emulated system on that host, not hardware.

## Adding a boot assertion

Keep the guest command in the existing serial input sequence. Add the assertion
to the matching architectural group using `require_marker`, `require_regex`,
`forbid_regex`, `require_count`, or `require_min_count`. Give every assertion a
stable dotted name and, when useful, an anchor matching the initiating command.
Do not add another monolithic condition or remove stress iterations to reduce
host output.
