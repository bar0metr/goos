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

`make test` runs host unit tests and validates the EFI artifact. `make
test-boot` boots headlessly, waits at most 20 seconds for `BOOT_OK`, and writes
the serial transcript to `build/logs/boot.log`.
`make test-performance` writes the page-allocation benchmark to
`build/reports/page-allocation.txt`.
`make test-security` runs the capability, generation-safe handle, and virtual
memory permission tests explicitly; these tests are also included by `make
test`.

Set `OVMF_CODE=/path/to/firmware.fd` if QEMU firmware is not installed under
`/opt/local/share/qemu`. Set `SOURCE_DATE_EPOCH` to a fixed project release
timestamp for release builds.
