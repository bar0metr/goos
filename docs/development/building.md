# Building and debugging

Run `make toolchain-check` before building. It validates dependencies without
installing or downloading anything. `make iso` creates all generated files
under `build/`, including `build/kernel/goos.efi` and
`build/goos-x86_64.iso`.

`make run` boots the ISO with 512 MiB in QEMU and attaches COM1 to the current
terminal. `make run-debug` adds `-s -S`: QEMU waits for a debugger on TCP port
1234 before executing the first instruction.

`make test` runs host unit tests and validates the EFI artifact. `make
test-boot` boots headlessly, waits at most 20 seconds for `BOOT_OK`, and writes
the serial transcript to `build/logs/boot.log`.
`make test-performance` writes the page-allocation benchmark to
`build/reports/page-allocation.txt`.

Set `OVMF_CODE=/path/to/firmware.fd` if QEMU firmware is not installed under
`/opt/local/share/qemu`. Set `SOURCE_DATE_EPOCH` to a fixed project release
timestamp for release builds.
