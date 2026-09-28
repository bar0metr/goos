# Building and debugging

Run `make toolchain-check` before building. It validates dependencies without
installing or downloading anything. `make iso` creates all generated files
under `build/`, including `build/kernel/goos.efi` and
`build/goos-x86_64.iso`.

## Toolchain and pinned versions

`tools/toolchain.versions` pins every external tool (ADR 0086):

| Tool | Pinned | On a different version |
| --- | --- | --- |
| Go | 1.25.5 | error: it compiles the binaries |
| TinyGo | 0.42.0 (LLVM 22.1.4) | error: it compiles the binaries |
| clang (Xcode Command Line Tools) | Apple clang 21.0.0 | warning: assembles two `.S` files |
| make | GNU Make 3.81 | warning |
| `qemu-system-x86_64` | 11.0.1 | warning: runs the result only |
| OVMF `edk2-x86_64-code.fd` | SHA-256 of MacPorts qemu 11.0.1 | warning |

`make toolchain-check` prints `[ OK ]`, `[MISS]`, `[FAIL]` (an error pin
differs), or `[WARN]` for each tool, `nm`, and the base-system `file`, `nc`,
and `shasum` the tests use, with a macOS install hint for each problem: the
go.dev package and TinyGo release archive for the pinned versions,
`xcode-select --install`, `sudo port install qemu`, or `brew install qemu`. It
also checks that the pinned Go matches the `go` directive in `go.mod`. It never
installs anything and exits non-zero on a missing tool or an error pin. The
build runs Go with `GOTOOLCHAIN=local`, so the go command never downloads a
different toolchain. Move a pin only after `make test-reproducible` and
`make test-all` pass on the new version.

The build needs no disk-image tool: `tools/mkiso` writes the FAT16 EFI system
partition (`build/variants/<config>/iso/efi.img`) and the ISO with its UEFI El
Torito entry.

## Reproducible builds

Two builds of the same sources are byte-identical. Every timestamp in the
images is `SOURCE_DATE_EPOCH` (default 0), archive and directory entries are in
name order, and nothing records the host, the time, or a random value.
`make test-reproducible` checks it: it cleans `build/` (keeping disks, logs,
and reports), builds the ISO, sets the outputs aside in `build/reproducible/`,
cleans again, caches included, builds again, and compares the SHA-256 of every
artifact. A difference names each file that differs and leaves both trees in
`build/reproducible/`. It honours `HZ`, `DIAG`, and `VALIDATION`, and leaves the
next build to start cold, as `make clean` does.

## Building, running, and testing

The userspace build compiles `userspace/cmd/init`, `userspace/cmd/vfs`, and
`userspace/cmd/shell` with the dedicated
`goos-userspace-amd64` TinyGo target, links a static ELF at the native userspace
base, and converts its load segments to `build/userspace/init`. ELF is an
intermediate build format only; the kernel accepts the GoOS native envelope.

Each userspace program is rebuilt when any file of a project package it
imports changes. The Makefile asks `go list -deps` for those packages under the
userspace target's build tags rather than keeping the lists by hand; hand lists
had drifted, and an edit to `kernel/mm/vm` once left the filesystem service
built against the old image window. The kernel depends on everything under
`kernel/` and `internal/`.

Before the conversion, `tools/stackframes` disassembles each userspace ELF
with `go tool objdump` and fails the build if a function's stack frame is deep
enough to step over the 64 KiB guard below the stack, or changes the stack
pointer by an amount it cannot bound. It also checks the kernel as soon as it
is linked, against the kernel stacks' 8 KiB guard. Each check prints the
largest frame of the binary; `go run ./tools/stackframes -v BINARY` lists
every frame (ADR 0088).

`make run` boots the ISO headlessly with 512 MiB in QEMU and attaches COM1 to
the current terminal. `make run-gui` uses the same OVMF, ISO, persistent disk,
q35 machine, and COM1 connection, but also opens QEMU's VGA window for manual
framebuffer-console validation. `MEMORY=4G` gives the guest more RAM and
applies to either target and every QEMU test. `make run-debug` adds `-s -S`:
QEMU waits for a debugger on TCP port 1234 before executing the first
instruction.

`make test` runs the fast host unit, artifact-integration, and security targets
without starting QEMU. `make test-boot` performs the bounded two-phase QEMU boot
and persistence validation. It keeps the complete serial transcripts in
`build/logs/boot.log` and `build/logs/reboot.log` while printing only grouped
PASS lines or a bounded diagnostic for the first failed named check. `make
test-all` is the canonical complete Definition-of-Done gate and includes host
tests, lint, documentation checks, ISO construction, and QEMU validation. See
`docs/development/testing.md` for the target hierarchy and harness details.
Boot tests use a disposable test disk; `make run` and `make run-gui` retain
`build/virtio-blk.img` across interactive boots.
`make test-performance` writes the host benchmarks, such as
`build/reports/page-allocation.txt`, and the in-guest microbenchmarks of
`make test-microbench` to `build/reports/microbench-<variant>.md`.
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
`build/reports/latency-<variant>.md`; see `docs/development/testing.md`.

`OPT` sets TinyGo's optimisation level for the kernel and every userspace
image: `2` (the default, TinyGo's fastest; there is no `-O3`), `1`, `s`, or `z`
(the smallest code). ADR 0090 records why the default is `2`. Each level
builds into its own variant directory (`build/variants/hz100-opt2`,
`build/variants/hz100-optz`), and `make test-latency` and `make test-microbench`
name their reports after it (`build/reports/microbench-hz100-optz.md`), so two
levels can be measured side by side.

`VALIDATION=1` builds the validation variant (ADR 0058): kernel self-tests,
fault injection, PID 1's supervision scenario, and the `*_OK` markers the boot
test asserts. A normal build runs none of them and prints only the banner,
service status, and prompt. `make test-boot` always builds the validation
variant.

Each configuration builds into its own tree, `build/variants/hz<rate>`, with
`-diag` and `-validation` suffixes, so switching `HZ`, `DIAG`, or `VALIDATION`
never mixes objects from different configurations and returning to a built
configuration does not rebuild it. `make iso` copies the requested variant to
`build/goos-x86_64.iso` and records its values in `build/config/build` for the
test harnesses.

Set `OVMF_CODE=/path/to/firmware.fd` if QEMU firmware is not installed under
`/opt/local/share/qemu` (Homebrew installs it under
`$(brew --prefix)/share/qemu`). Set `SOURCE_DATE_EPOCH` to a fixed project
release timestamp for release builds; the kernel build and the timestamps in
the EFI partition and the ISO take it.

## Release version

The release is written once, as `Release` in `internal/version/version.go`.
The kernel banner, `uname`, the read-only `system.build.release` setting, and
`make version` all take it from there, and the boot test reads the expected
banner and `uname` line from the same file. Changing the version is that one
edit followed by `make`.
