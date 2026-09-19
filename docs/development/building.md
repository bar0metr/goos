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
`make test-performance` writes the page-allocation benchmark to
`build/reports/page-allocation.txt`.
`make test-security` runs the capability, generation-safe handle, and virtual
memory permission tests explicitly; these tests are also included by `make
test`.

Set `OVMF_CODE=/path/to/firmware.fd` if QEMU firmware is not installed under
`/opt/local/share/qemu`. Set `SOURCE_DATE_EPOCH` to a fixed project release
timestamp for release builds.
