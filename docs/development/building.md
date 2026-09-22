# Building and debugging

Run `make toolchain-check` before building. It validates dependencies without
installing or downloading anything. `make iso` creates all generated files
under `build/`, including `build/kernel/goos.efi` and
`build/goos-x86_64.iso`.

The userspace build compiles `userspace/cmd/init`, `userspace/cmd/vfs`, and
`userspace/cmd/shell` with the dedicated
`goos-userspace-amd64` TinyGo target, links a static ELF at the native userspace
base, and converts its load segments to `build/userspace/init`. ELF is an
intermediate build format only; the kernel accepts the GoOS native envelope.

`make run` boots the ISO with 512 MiB in QEMU and attaches COM1 to the current
terminal. `make run-debug` adds `-s -S`: QEMU waits for a debugger on TCP port
1234 before executing the first instruction.

`make test` runs the fast host unit, artifact-integration, and security targets
without starting QEMU. `make test-boot` performs the bounded two-phase QEMU boot
and persistence validation. It keeps the complete serial transcripts in
`build/logs/boot.log` and `build/logs/reboot.log` while printing only grouped
PASS lines or a bounded diagnostic for the first failed named check. `make
test-all` is the canonical complete Definition-of-Done gate and includes host
tests, lint, documentation checks, ISO construction, and QEMU validation. See
`docs/development/testing.md` for the target hierarchy and harness details.
Boot tests use a disposable test disk; `make run` retains
`build/virtio-blk.img` across interactive boots.
`make test-performance` writes the page-allocation benchmark to
`build/reports/page-allocation.txt`.
`make test-security` runs the capability, generation-safe handle, and virtual
memory permission tests explicitly; these tests are also included by `make
test`.

## Build configuration

`HZ` selects the timer rate for every target: `make HZ=250 run` or
`make HZ=1000 test-boot`. The default is 100 Hz; 250 and 1000 are the supported
diagnostic rates, and any other value is rejected. The rate is a build tag
(`goos_hz250`, `goos_hz1000`) that selects the `kernel/clock` constant, so the
kernel, the PIT reload value, and every userspace deadline change together
without editing Go or assembly. The boot log prints the active rate as
`Timer: <rate> Hz`.

`DIAG=1` builds the diagnostic variant (ADR 0052). It records a bounded trace
of wait, channel, handle-transfer, and process events and lets `diag` in the
shell print the kernel state. Services and PID 1 print the same dump when they
stop on a fatal error. A normal build compiles all of this out and `diag`
reports that it is unavailable.

`make HZ=<rate> test-latency` measures command latency at one rate and writes
`build/reports/latency-hz<rate>.md`; see `docs/development/testing.md`.

`build/config/build` records the active `HZ` and `DIAG` values. Changing either
rebuilds the kernel and userspace; repeating a build with the same values does
not.

Set `OVMF_CODE=/path/to/firmware.fd` if QEMU firmware is not installed under
`/opt/local/share/qemu`. Set `SOURCE_DATE_EPOCH` to a fixed project release
timestamp for release builds.
