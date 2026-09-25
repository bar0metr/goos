# Testing

GoOS keeps fast host validation separate from the complete QEMU gate.

## Targets

- `make test-unit` runs all host-side Go tests.
- `make test-integration` builds and validates the UEFI kernel artifact.
- `make test-security` runs the capability, handle, IPC, VM, filesystem, and
  userspace security-focused host tests explicitly, and those of the
  build-time stack frame check (`tools/stackframes`, ADR 0088).
- `make test` runs the three fast targets above and does not start QEMU.
- `make test-boot` builds the validation ISO (`VALIDATION=1`, ADR 0058) and
  runs the two-phase QEMU boot and reboot persistence test.
- `make test-all` is the canonical complete Definition-of-Done gate. It runs
  the fast tests, lint, documentation checks, the ISO build, and the real QEMU
  boot/persistence test in sequence so host builds cannot contend with QEMU.
- `make test-stress` boots `STRESS_RUNS` guests (default 4) in parallel
  batches of `STRESS_JOBS` and drives `STRESS_ROUNDS` rounds of commands
  through each; `make test-all` includes two runs.
- `make test-storage-stress` runs the FAT32 host workload at scale: a seeded
  model-based sequence of creates, writes, truncations, moves, and removals
  checked against a model and a consistency checker, device-failure injection,
  a file past 16 MiB, and the host's own FAT checker (`fsck_msdos` or
  `fsck.fat`, when installed) over volumes the filesystem built (ADR 0102). The
  default `make test-unit` runs the same tests at a smaller scale.
- `make test-latency` boots one guest and measures command and VFS latency
  into `build/reports/latency-<variant>.md`, where `<variant>` is the build's
  directory under `build/variants/` (`hz100-opt2` by default). It is a
  measurement, not a gate, and is not part of `make test-all`.
- `make test-microbench` boots one guest and runs the in-guest
  microbenchmarks (context switch, IPC, VFS reads, RAMFS metadata) into
  `build/reports/microbench-<variant>.md` (ADR 0085). Like `test-latency` it is
  a measurement, not a gate.
- `make test-performance` runs `test-microbench` and then the host
  microbenchmarks, writing every report under `build/reports/`.
- `make lint` runs `go vet`, the `gofmt` check, and
  `tests/lint/unsafe_boundary.sh`, which refuses `unsafe` in the kernel outside
  `uefi`, `vm`, and `amd64`, and `unsafe`, `//go:linkname`, and `//go:extern` in
  userspace outside `userspace/runtime` (ADR 0095). The script tests itself
  first by planting each forbidden form in a scratch tree.
- `make docs` checks that the required documents exist and runs
  `tests/docs/check.sh`: every relative Markdown link resolves, every ADR has
  a status and a decision, and the current release has notes.
- `make release-check` is the release gate. It runs `test-reproducible`
  first — which cleans `build/` and builds twice, so a clean build is part of
  the gate — then lint, docs, `make test`, `test-storage-stress`, the ISO, `tests/release/check.sh`,
  `test-boot`, and a two-guest `test-stress`, and the release checks once more
  at the end. The release checks require notes for the version in
  `internal/version/version.go`, a kernel that reports it, no validation marker
  or fault-injection report in any normal image, no retired `DebugWrite` stub,
  and no untracked or ignored file outside `build/`. Timing measurements are
  not part of it: they depend on the host.

When comparing timings between two trees, put both disk images on the same
host volume: QEMU writes the image file synchronously, so a slower volume
shows up as slower persistent writes (ADR 0096).

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

The storage group asserts that the driver, which the validation build kills once
after the system is up, starts twice and that the block service is told about
the replacement and does not itself restart.

A fourth QEMU phase boots the system on a volume claiming far more sectors than
its device holds, logged to `build/logs/corrupt-volume.log`. It requires the
system to reach a prompt, report the refused volume, list a root without the
persistent mounts, run a command and answer a configuration query: a corrupt
disk must cost the storage, not the system.

The config group also asserts that a setting is actually consulted: changing the
prompt applies on the next line and is then restored, and the log shows two
sessions created with different rights. That a confined session cannot reach
outside its scope is covered by the store's unit tests and the control
protocol's round trip; proving it from inside the system would need a client
built to try, which is a fixture rather than a component.

The config group asserts that a value is checked against the type its key
declares, that a published value cannot be rewritten, that an undeclared key is
not a new setting, that the exported file holds the writable entries and not the
published ones, and that a value set in one session is restored in the next.

The release-shell group also asserts that the shell's CPU total stays in three
digits and that no console input was dropped, which is the regression guard on
the interrupt-driven console: before it, the shell was charged over seven
thousand ticks in a session.

The release-shell group asserts the resource accounting: that `ps` reports
non-zero pages for PID 1, that `quota` shows an ordinary process holding a share
of the stream table rather than all of it, and that PID 1 holds the system
share. The `QUOTA_OK` marker comes from a validation-build check that drives the
same charge-then-create order `StreamCreate` uses and requires the refusal to
arrive while registry slots remain.

The release-shell group also asserts the terminal session (ADR 0089). The
shell's capability list names a write-only `stdout` stream and no console, and
the shell checks in the validation build that the kernel refuses a read on it
(`TERMINAL_STREAM_READ_DENIED_OK`). A line typed into `readline` has to be
printed by it and never run as a command. `fault tty` and `fault shell`, a
validation-only shell command that asks PID 1 to kill the service, restart each
once: the terminal's replacement has to serve the running shell's next command,
the shell's replacement has to be served by the running terminal, and the
background job the old shell left has to be gone. The ipc-capabilities group's
`STREAM_SHARED_ENDPOINT_OK` comes from PID 1 sending itself a writer attached
twice and checking that the end stays open until both copies close, and that
closed and wrong-direction handles are refused.

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
  VFS session. Each prints `bench <mode>: N ops, total <us> us, avg <ns> ns/op`;
  `read` adds the file size and the rate. N is at most 10000. A file under
  `/persist` exercises VFS, FAT32, the block service, and the virtio-blk
  driver. The other modes are described
  under [In-guest microbenchmarks](#in-guest-microbenchmarks).

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
transcript is kept in `build/logs/latency-<variant>.log`. In a `DIAG=1` build
the script ends with
`diag` and adds the `DIAG clock` and `DIAG launch` lines to the report: per
stage of `spawnExecutable` (image authority and extent check, address-space creation, image load,
process and grant setup, task install), the number of launches since boot and
their total and average time. Failed lookups are counted too. Numbers from
QEMU TCG describe the emulated system on that host, not hardware.

## Performance measurement

The specification (§51) asks for microbenchmarks of the context switch, IPC
ping-pong and throughput, shared-memory throughput, page allocation, page
faults, cached and uncached VFS reads, and RAMFS metadata operations, with
results in `build/reports/`, and for no optimisation on assumptions.
`make test-performance` produces two kinds of report.

Host benchmarks (`go test -bench`) time the kernel data structures on the host:
`page-allocation.txt` (`pmm` allocate/free), `ipc-ping-pong.txt` (channel
send/receive in one address space), `shared-memory.txt`, `stream-roundtrip.txt`,
`ramfs-metadata.txt`, and `vfs-resolve.txt`. They measure the algorithms, not
the system: no trap, no context switch, no second process.

### In-guest microbenchmarks

`make test-microbench` (ADR 0085) boots one normal-build guest, runs each case
of `tests/boot/microbench.sh` once to warm up and `MICROBENCH_REPEATS` times
(default 5), and writes `build/reports/microbench-<variant>.md` with the median
over the runs and the raw `bench` lines. The transcript is kept in
`build/logs/microbench-<variant>.log`.

| Case | Command | What it measures |
| --- | --- | --- |
| IPC ping-pong | `bench vfs 1000` | a request/reply on a channel between the client and the VFS service, including the service's `Stat /` |
| Stream calls, one process | `bench stream N SIZE` | write, drain, and empty read of a private stream plus a clock read per operation; no switch |
| Stream handoff | `bench send 200 1 \| bench recv 200 1` | one-byte round trips between two processes |
| IPC throughput | `bench send N SIZE \| bench recv N SIZE` | bytes per second through a two-stage pipeline |
| Cached VFS read | `bench read /system/bin/uname 50` | a 10 KiB file of the memory-resident system image |
| Uncached VFS read | `bench read /persist/uname.bin 20` | the same bytes on FAT32; file data is not held in the service's sector cache, so each pass reads the device |
| RAMFS metadata | `bench meta /tmp/bench.tmp 500` | create and unlink of one file in the RAMFS `/tmp`, two VFS requests |

`bench recv` times from its first chunk to its last and reads the clock after
every chunk. While it is blocked on the empty stream, every write wakes it and
the kernel switches to it on the write's return (ADR 0055); it drains the chunk
and blocks again. A chunk preceded by exactly one wait is therefore one round
trip, and the report gives the median of those intervals separately from the
average. The sender never blocks, so a timer tick that preempts the receiver
lets the sender fill the stream and the pair then moves many chunks per switch:
that is the throughput case, and it is why the handoff case is short.

The context switch is derived: (handoff op median - one-process op median) / 2,
both at one byte and both with one clock read per operation. The difference is
one switch between two address spaces plus half of a blocking wait, its wake,
and the retried wait.

```sh
make test-microbench
make HZ=1000 test-microbench MICROBENCH_REPEATS=9
```

Page faults and shared-memory throughput between processes are not measured in
the guest; ADR 0085 records why. The guest TSC advances in steps of about 1 µs
under TCG on an arm64 host, so single intervals are quantised to that step.
Other QEMU guests running on the same host change the results by tens of
percent; rerun on a quiet host before comparing builds.

## Adding a boot assertion

Keep the guest command in the existing serial input sequence. Add the assertion
to the matching architectural group using `require_marker`, `require_regex`,
`forbid_regex`, `require_count`, or `require_min_count`. Give every assertion a
stable dotted name and, when useful, an anchor matching the initiating command.
Do not add another monolithic condition or remove stress iterations to reduce
host output.
