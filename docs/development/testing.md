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
- `make test-system-boot` runs the installed-system and recovery proofs
  (ADR 0175); see [the system boot test](#system-boot-test).
- `make test-all` is the canonical complete Definition-of-Done gate. It runs
  the fast tests, lint, documentation checks, the ISO build, the real QEMU
  boot/persistence test, and the Linux end-to-end boot in sequence so host
  builds cannot contend with QEMU.
- `make test-linux-elf` and `make test-linux-abi` run the Linux host tests:
  the ELF parser against hostile and real images with every fixture's
  checksum, and the pure translation code with the VFS protocol it speaks.
- `make test-linux-boot` builds the validation ISO and runs
  `tests/boot/linux.sh`: one boot in which PID 1's Linux probes run, then the
  shell runs the release proofs, a static PIE, the conformance probe's
  hardening modes, a stack overflow into the guard, two Linux processes at
  once, Ctrl-C on a blocked Linux process and on a Linux root with a forked
  child, and the process-composition modes: `exec`, `fork`, `vfork`,
  `orphan`, `wait`, `pipe`, `signal`, `kill`, `eintr`, `poll`, `ppoll`,
  `hardening`, `fpstate`, and `limits`. Last come two injected failures: a fork that
  fails mid-copy and an `execve` whose VFS dies (ADR 0173). Its guest CPU has RDRAND
  (`-cpu qemu64,+rdrand`), so `getrandom` must work there; the general boot
  test keeps the default CPU, where it must return `ENOSYS`.
  `test-linux-musl`, `test-linux-vfs`, `test-linux-security`,
  `test-linux-process`, `test-linux-exec`, `test-linux-fork`,
  `test-linux-pipe`, `test-linux-signals`, and `test-linux-poll` are the same
  run. `make test-linux-all` adds the two host targets. `make test-all`
  includes the boot.
- `make test-linux-reference` runs the conformance probe on a Linux kernel in
  an amd64 container and requires exit status zero. It needs Docker and is no
  part of `test-all`. On an arm64 host the container runs under user-mode
  emulation, which answers a few hostile calls itself; the probe is lenient
  there.
- `make test-linux-kernel-reference KERNEL=/path/to/vmlinuz
  BUSYBOX=/path/to/busybox` boots a real Linux kernel in QEMU with the probe
  in an initramfs and requires the default, hostile, and process modes to
  pass strict; `make test-linux-process-reference` is the same run:
  every check must give the exact Linux result. An Alpine `vmlinuz-virt`
  works as it comes. The repository carries neither file, so the target is
  no part of `test-all`.
- `make test-linux-busybox BUSYBOX=/path/to/busybox`, also
  `make test-linux-shell`, places a static x86_64 BusyBox in the validation
  image and requires `echo`, `pwd`, `cat`, `ls`, `mkdir`, and `rm`, and a
  non-interactive `sh` that runs programs, pipelines, redirections, a script,
  and a background job; it reports on further applets without requiring them.
  With `DIAG=1` it also prints the kernel's record of refused calls. The
  repository carries no BusyBox.
- `make test-linux-bench` times four Linux calls beside native `bench` cases
  into `build/reports/linux-abi-<variant>.md`. `QEMU_ACCEL=kvm` on an x86_64
  host times the guest on the host CPU; the default is `tcg`.
- `make test-stress` boots `STRESS_RUNS` guests (default 4) in parallel
  batches of `STRESS_JOBS` and drives `STRESS_ROUNDS` rounds of commands
  through each; `make test-all` includes two runs.
- `make test-fat32` runs the FAT32 format tests (ADR 0102, ADR 0112): every
  logical sector size and cluster size with one and two FATs, BPB validation
  and backup-boot recovery, the FSInfo free count, FAT mirror comparison,
  maximum-length ASCII, Cyrillic, CJK, emoji and mixed Unicode names, complete
  simple case mapping and collisions, malformed UTF-16, primary/backup FSInfo
  fallback and reconciliation, timestamps and attributes, 1, 16 and
  64 MiB files, fragmentation, a directory of long names, a corruption corpus
  and seeded random corruption, and a model-based workload with device-failure
  injection. Where the host has FAT tools it also formats volumes with them
  (`newfs_msdos` or `mkfs.fat`) and mounts, fills and hands them back to the
  host checker (`fsck_msdos` or `fsck.fat`); on macOS the host's own FAT driver
  writes files this code reads and reads back files this code writes. Two
  host-made volumes in `userspace/fat32/testdata` cover foreign volumes on
  every host; `GOOS_FAT32_FIXTURES=1 go test -run TestMakeForeignFixtures
  ./userspace/fat32` regenerates them on macOS. Tests that need a host tool
  skip without it. `make test-unit` includes all of this.
- `make test-partitions` runs the partition-layer and block-slice tests (ADR
  0115): MBR and GPT parsing at 512 and 4096-byte blocks, every selector, GPT
  backup fallback and each header, array and entry corruption, seeded random
  corruption, slice bounds and read-only slices. On macOS `hdiutil` makes MBR
  and GPT disks whose FAT32 partition this code mounts and writes, and the
  host's `gpt`, `fdisk`, `hdiutil` and `fsck_msdos` read, mount and check the
  tables and volumes this code writes; two host-made disks in
  `userspace/partition/testdata` cover foreign tables everywhere
  (`GOOS_PARTITION_FIXTURES=1` regenerates them). The boot test boots from an
  MBR and a GPT disk and reads the guest's file back out of the partition.
- `make test-storage-stress` (`make test-fat32-stress`) runs the same tests at
  scale: 128 KiB to 512 KiB clusters, a 1 GiB and a 4 GiB − 1 file on a sparse
  host file, thousands of seeded corruptions, a directory of 8000 long names,
  and a longer model-based workload.
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

`make test-boot MEMORY=4G` runs the same test with more guest RAM (default
`512M`). The physical-memory group then requires the validation kernel to have
used a page above 1 GiB from 2 GiB, and one above 4 GiB from 3 GiB (ADR 0103).

The target creates a fresh `build/boot-test-virtio-blk.img` for each run. Tests
therefore cannot corrupt or overwrite the persistent `build/virtio-blk.img`
used by `make run` and `make run-gui`; both QEMU phases share the fresh test image to validate
reboot persistence.

`make run-gui` is a manual target and is never invoked by the automated test
hierarchy. It opens a host window, while `make test-boot` remains headless and
does not require Cocoa, GTK, SDL, or another host display backend. The manual
target defaults to `QEMU_DISPLAY=cocoa` and permits a host-specific override.
The boot test asserts the serial `FRAMEBUFFER_TERMINAL_OK` initialization marker. It also
requires `PS2_KEYBOARD_READY`, proving QEMU acknowledged the bounded i8042
initialization before userspace starts.
It requires `FRAMEBUFFER_USER_ISOLATION_OK` after constructing both validation
process page-table roots, proving neither root inherited the kernel framebuffer
slot.

`make test-console-gui` is the automated framebuffer test. It creates a fresh
disk, boots the validation ISO with standard VGA and `-display none`, captures
serial output, and waits for `FRAMEBUFFER_VALIDATION_PATTERN_OK`. It then sends
`screendump` over a private QEMU monitor socket and verifies the exact
magenta/cyan 16x16 checkerboard in the lower-right corner of the resulting PPM.
The test then resumes the guest and injects editing keys plus the mandatory
`echo graphical-keyboard-ok` command through the private QEMU monitor. The
keys reach the framebuffer console's session, `tty-fb` and `shell-fb`
(ADR 0125). COM1 is a file sink; the validation build lets `tty-fb` copy its
output there from the first key, so the `PS2_KEYBOARD_OK` marker and exact
command output prove the input traversed IRQ1, the decoder, the keyboard ring,
`tty-fb`, and `shell-fb`. The test
also runs `mount` and `df` through that framebuffer session and requires their
validation markers, proving the filesystem-information commands use the same
terminal-independent command path. The test
also requires `FRAMEBUFFER_USER_ISOLATION_OK`; it does not inspect text or
require OCR. Its log and image are retained as
`build/logs/console-gui.log` and `build/reports/console-gui.ppm`.

`go test ./userspace/tui` drives the reusable TUI layer with a host model
terminal. It checks clipped geometry, the protected bottom-right cell, dirty-row
suppression, list/dialog layout at 80x24, text scrolling and overlays, rejected
dimension overflow, and zero allocations while changed steady-state frames are
rendered and flushed.

`go test ./userspace/viewer` uses in-memory and synthetic readers without a
filesystem-sized allocation. It covers line/page/Home/End navigation, a 1 GiB
synthetic file, lines larger than the 4 KiB read window, horizontal clipping,
binary detection, C0/ESC sanitization, invalid UTF-8, read failure, status-path
truncation and allocation-free changed frames. The TinyGo `gview` image is also
built through the normal command target.

The same run then drives the file manager through IRQ1 only (ADR 0124): `cd
/tmp`, `gfm`, F1 and Escape, Tab, arrows, Home, End, PageDown, PageUp, F7 with
a typed name, and F10. `tty-fb` copies `gfm`'s ANSI output to COM1, so the log
shows its key bar, help and status text; after it exits, `GFM_EXIT_OK`,
`GFM_RAW_INPUT_OK`, `GFM_FUNCTION_KEYS_OK`, `GFM_MKDIR_OK`, `TTY_RAW_MODE_OK`
and `SHELL_TERMINAL_RESTORED_OK` prove the path from PS/2 through the decoder,
the ring, raw mode, the shell and the key parser, and `ls /tmp` listing
`fmtest/` proves the shell reads lines again. A second `gfm` is stopped with
Ctrl-C, and `echo after-gfm-ok` must still run. A screendump of the manager is
kept as `build/reports/gfm.ppm` for review.

The graphical scenario then opens `/system/motd` in `gview`, sends page,
horizontal, Home and End keys, exits with F10 and verifies line-mode shell input
afterwards. The COM1 boot scenario opens the same file, sends navigation and F10
as raw ANSI input, and likewise verifies the restored shell. These are the two
terminal-session acceptance paths; no viewer-specific framebuffer or keyboard
path exists.

Both scenarios then run the 0.1.7 workflow end to end. On the framebuffer the
test creates `/home/gv/note`, starts `gfm` there, views the file with F3,
returns with F10, edits it with F4, types `X`, saves with F2 and leaves the
editor and the manager with F10; `cat` must print `Xhello`. `GVIEW_EXIT_OK`,
`EDIT_SAVE_OK`, `EDIT_EXIT_OK`, `GFM_LAUNCH_OK` and a new
`SHELL_TERMINAL_RESTORED_OK` prove each step reached the program that owned the
keys. The COM1 boot test sends the same keys as raw ANSI on `/home/fm/note.txt`,
checks `Xsaved text` and checks that `ls` shows no `.note.txt.~save` left by
the save.

`go test ./userspace/safesave ./userspace/fat32 ./kernel/vfs` covers safe
replacement: every save step fails in turn and the original survives, and a
FAT32 replacement is cut at each of its device writes, remounted and checked to
hold one whole version with at worst lost clusters (ADR 0129).
`go test ./userspace/exec ./userspace/init` covers the launch protocol and
PID 1's policy for launched children (ADR 0130), and `go test
./userspace/editor ./userspace/filemanager` the editor model and `gfm`'s
viewer, editor, date and capacity handling on model terminals.

The boot test drives the COM1 session and never types at the keyboard, so
`tty-fb` copies nothing to COM1 and the test's marker counts see the COM1
session alone.

Host tests cover the rest of the stack: every F key's make and break codes and
exact sequence, the E0 navigation keys and the eight-byte bound
(`kernel/keyboard`); multi-parameter CSI, CUP, CHA, erase variants, all SGR
colours, split and malformed sequences, and padding guards
(`kernel/framebuffer`); the key parser, output builder and width helpers
(`userspace/terminal`); the raw-mode protocol, raw input buffer and reset
sequence (`userspace/tty`); and the manager's sorting, movement, scrolling,
refresh, dialogs, copy, move, mkdir and delete, error paths, layout at several
geometries and allocation-free rendering, against a model filesystem and a
model terminal that fails on any write past a row or into the scrolling corner
(`userspace/filemanager`).

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
non-zero pages for PID 1, that `quota` shows its own `tiny` share
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
mount, that `/system` is the boot image with its utilities listed under
`/system/bin`, that `mkdir /system/backdoor` is refused with "read-only
filesystem", and that a file written through `/home` reads back through
`/persist/home` as the same bytes. It executes `mount` and `df`, checks the
system-image and persistent-volume rows, and verifies the capacity columns.

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

## System boot test

`make test-system-boot` (`tests/boot/system.sh`) boots the validation ISO
against disposable disks made from the build's system tree, types commands
at the serial prompt, and powers each guest off. It checks:

1. the production archive, built from the same inputs without the
   validation probes, holds the storage services, the shell, and the recovery
   commands and none of `gfm`, `bench`, `uname`, the Linux fixtures,
   `fault-probe`, `config`, or `log`; and `make run-disk` leaves an existing
   disk byte for byte as it was while `make disk` refuses it;
2. a normal boot starts `config`, `log`, and `shell` from the volume
   (`SERVICE_FROM_DISK_OK`), runs `uname`, which only the volume has
   (`SYSTEM_COMMAND_FROM_DISK_OK`), and gives the shell
   `PATH=/system/bin:/home/user/bin`;
3. native commands cannot change the installed system through `/system`,
   `/persist/system`, case variants, rename, copy, or a moved directory, and
   the Linux `fsprobe` cannot open it for writing under either name; a copy of
   `fsck-fat` in `/home/user/bin` gets no storage slice, and `fsck-fat` in
   normal mode is refused as busy without unmounting anything; pipelines,
   redirection, and a Linux program still work;
4. after a reboot of the same disk the user's file is there and a copied
   command runs from `/home/user/bin` by its bare name without grants; after
   `fault fat32`, `fault vfs`, and `fault driver` the system is attached
   again, still protected, and runs commands from the volume; `fault storage`
   ends in recovery mode with one shell per console;
5. a forced-recovery ISO checks the installed volume with `fsck-fat` and
   mounts it again afterwards;
6. a missing disk, a blank volume, a malformed record, an older release, a
   missing shell image, and a malformed index each boot to a recovery prompt
   within 60 seconds with the stage and reason printed, where the archive's
   commands work; and recovery can `edit` a file and check the volume.

The transcripts are `build/logs/system-*.log`.

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
