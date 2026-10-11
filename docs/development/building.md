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

## Native crypto probes

`make crypto-check` builds isolated native crypto probes under the pinned tools.
`make test-crypto` runs the successfully built probes in a disposable validation
guest. `CRYPTO_COMPAT=1` selects the opt-in native runtime overlay; it does not
modify the installed toolchain or the kernel runtime. Reports and temporary
crypto work trees belong under `build/tmp/`. Scratch allocators create a nested
module boundary there so repository-wide Go tests skip copied toolchains.
See [native crypto feasibility](crypto-feasibility.md)
for package classifications, measurements and the stock TinyGo no-op TLS result.
The selected native HTTPS path instead compiles the pinned Go software TLS
implementation.

## Native HTTPS and system roots

Native `fetch` uses Go 1.25.5 software TLS/X.509 under TinyGo 0.42.0.
`tools/cryptocompat` and `tools/tlscompat` prepare local copy-on-write toolchain
roots under `build/tmp/`, checking pinned source hashes and recording
adapter/output manifests. They do not download or modify installed tools, and
only the userspace client uses this runtime. The compatibility layer supplies
kernel entropy and native clocks, rejects cancelable contexts/QUIC, disables
weak certificate retention, and adds bounded certificate/chain checks.
Native TLS consumers call `arena.InitPackages` after heap setup so the pinned
runtime runs `initAll`; other native entry points keep their existing startup.
See
[ADR 0198](../adr/0198-native-tls-and-system-trust.md).

Normal system trees select `security/ca-certificates.pem`. Its 121 roots occupy
181603 bytes; [security/README.md](../../security/README.md) records the pinned
Mozilla/curl 2026-09-25 source, digests and MPL 2.0 license. Regenerate or check it
using only local inputs:

```sh
go run ./tools/truststore -sequence 2026092501 -source-date 2026-09-25T03:12:01Z -source-url https://curl.se/ca/cacert.pem security/ca-source.pem security/ca-certificates.pem
go run ./tools/truststore -check security/ca-source.pem security/ca-certificates.pem
```

`tools/sysimage` requires the explicit `SYSTEM_CA_BUNDLE` build input and
validates it before changing an output tree. The build selects the production
artifact by default and checks it against the pinned source. Only a
`VALIDATION=1` build with explicit `VALIDATION_CA_BUNDLE` may substitute the
public test root. Installed trust lives at
`/system/security/ca-certificates.pem` in the read-only system namespace.
Changing files on the host does not update an existing installed disk.

`make test-tls` supplies the fixture CA for the offline native HTTPS gate,
which passed 69 assertions. It also measures production roots with the
validation-only `trustprobe --production`; this fixed auxiliary input is not an
alternate CA path for fetch. The validation fetch image is 1779661 bytes and
its largest compiled frame is 20696 bytes (including unnegotiated ML-KEM code).
The production fetch image is 1776501 bytes and reproduces byte-for-byte.
Normal images exclude cryptoprobe and trustprobe. The stream network regression
passed, and the complete `release-check` matrix passed. The initial `test-all`
failed an obsolete boot directory-count assertion, corrected and verified by
the release boot gate. See
[release measurements](../releases/0.2.6.md).

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
the current terminal. It boots the installed system on `build/goos-disk.img`
(`DISK`), which it makes from the build's system tree when the file does not
exist and never changes when it does (ADR 0175). After a rebuild the disk
still holds the system it was installed with; a disk of another release boots
into recovery. `make disk` installs the system on a new `DISK` and refuses one
that exists; `make disk-overwrite` replaces it, user data and all.
The installer sizes a new FAT32 disk from the files in the system tree,
including `LINUX_APPS_DIR`: at least 512 MiB, with at least 5% more data
clusters than installation needs. `make run` does not resize an existing disk.
`make system-image` builds the ISO and a volume image of the installed system,
`$(OUT_DIR)/system.img`. `make run RECOVERY=1` boots an ISO whose archive asks
PID 1 for recovery mode, to inspect or check the disk. `make run-gui` uses the same OVMF, ISO, persistent disk,
q35 machine, and COM1 connection, but also opens QEMU's VGA window for manual
framebuffer-console validation. It uses the native macOS Cocoa backend by
default; set `QEMU_DISPLAY=gtk`, `QEMU_DISPLAY=sdl`, or another backend
available in the local QEMU build on other hosts. `MEMORY=4G` gives the guest
more RAM and applies to either target and every QEMU test. `SMP=4` gives
`make run`, `make run-gui`, and `make run-debug` one socket with four guest
CPU cores and one thread per core; it is unrelated to `make -j` build
parallelism. `make run-debug` adds `-s -S`: QEMU waits for a debugger on TCP
port 1234 before executing the first instruction.

### Testing Linux applications from a host directory

`LINUX_APPS_DIR` copies the contents of a host directory into `/apps` on a
newly built FAT32 disk. Subdirectories and file bytes are preserved. The
directory must be outside the generated system tree; symlinks and special
files are rejected. Run a static x86_64 ELF by its full path, for example
`/apps/tools/probe`. The Linux default `PATH` is `/system/linux`, so a program
in `/apps` is not found by its bare name.

Use a separate disposable disk for each build mode:

```sh
make test-disk TEST_DISK=build/linux-apps-clean.img LINUX_APPS_DIR="$PWD/linux-tools" DIAG=0
make run DISK=build/linux-apps-clean.img DIAG=0

make test-disk TEST_DISK=build/linux-apps-diag.img LINUX_APPS_DIR="$PWD/linux-tools" DIAG=1
make run DISK=build/linux-apps-diag.img DIAG=1
```

`DIAG=0` is the normal build. `DIAG=1` enables the guest's `diag` command and
kernel diagnostic counters and events; run `diag` in the guest and inspect
the serial output for refused Linux syscalls, faults and VFS failures. It does
not add a source debugger to the Linux program. For a paused QEMU GDB session,
use `make run-debug DISK=build/linux-apps-diag.img DIAG=1` instead of the
second `make run`; see [debugging.md](debugging.md#debugger).

`test-disk` replaces only the named disposable disk. Run it again after
changing files in `LINUX_APPS_DIR`; `make run` keeps an existing disk as it is.
The same command handles directories larger than 512 MiB; `mkfat32 -size auto`
prints the selected disk size before formatting. FAT32 still limits each
individual file to less than 4 GiB.
The default `build/goos-disk.img` is unaffected. `LINUX_EXTRA` has a different
purpose: it takes one file path, or several paths separated by the host's
`PATH` separator, and installs them under the read-only `/system/linux`.
Both variables also work with [Proxmox runs](proxmox-lab.md#runs).
The [application catalog](linux-application-compatibility.md) records tested
static Linux programs and their limits.

`make test-console-gui` performs the non-interactive counterpart: it keeps the
VGA device but uses `-display none`, captures a QEMU monitor screendump, and
checks a validation-only framebuffer marker pixel-for-pixel.

`make run` and `make run-gui` attach one legacy virtio-net function to QEMU
user networking: `-netdev user` and
`virtio-net-pci,disable-modern=on,mac=$(NET_MAC),addr=0x4`. The function sits
at PCI slot 4 because the Q35 firmware routes that slot's interrupt to a line
the disk does not use (ADR 0190). `NET=user` is the default; `NET=none`
boots without a network device, and any other value stops the build.
`NET_MAC` sets the MAC address, `52:54:00:12:34:56` by default. Under user
networking QEMU's DHCP server gives the guest an address in `10.0.2.0/24`
with gateway `10.0.2.2`, and `ping 10.0.2.2` reaches QEMU's router; reaching
further depends on the host's ICMP support.

The command index format is 3 since the network grants (ADR 0191). A disk
installed by an older build records format 2 and boots into recovery;
`make disk-overwrite` installs the current system on it, user data and all.

`make test` runs the fast host unit, artifact-integration, and security targets
without starting QEMU. `make test-boot` performs the bounded two-phase QEMU boot
and persistence validation. It keeps the complete serial transcripts in
`build/logs/boot.log` and `build/logs/reboot.log` while printing only grouped
PASS lines or a bounded diagnostic for the first failed named check. `make
test-all` is the canonical complete Definition-of-Done gate and includes host
tests, lint, documentation checks, ISO construction, and QEMU validation. See
`docs/development/testing.md` for the target hierarchy and harness details.
Boot tests use disposable installed disks made from the system tree; `make
run` and `make run-gui` retain `build/goos-disk.img` across interactive boots.
`make test-system-boot` boots an installed disk, reboots it under storage
faults, checks it from forced recovery, and boots each kind of unusable disk
into recovery. `make test-network` runs the network gate.
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

## Trust package maintenance

Normal builds check local PEM and `security/ca-certificates.json` offline. Build
inputs `TRUST_UPDATE_KEY` (raw public-key file) and `TRUST_UPDATE_URL` install
immutable `/system/security/trust-update-key.pub` and `trust-update-url`.
The production URL defaults to `https://bar0metr.github.io/current`; it remains
a build-time override. Publishing signed package data is an explicit maintainer
action, separate from building the OS. Validation uses separate
`VALIDATION_TRUST_UPDATE_KEY` and `VALIDATION_TRUST_UPDATE_URL` inputs.

`make trust-update TRUST_SOURCE=LOCAL_PEM TRUST_OUTPUT=OUTPUT.pem
TRUST_SEQUENCE=VERSION TRUST_SOURCE_DATE=UTC_RFC3339` normalizes and emits
canonical metadata. `TRUST_OLD_BUNDLE` reports root differences;
`TRUST_SIGNING_KEY` explicitly signs with an external raw private seed. The
ordinary build neither downloads source material nor generates signing keys.
See [security maintenance](../../security/README.md) for complete commands.
