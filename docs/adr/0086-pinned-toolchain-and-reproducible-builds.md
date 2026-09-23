# ADR 0086: Pinned toolchain and reproducible builds

## Status

Accepted. Replaces the `hdiutil` image recipe and `tools/iso-uefi` of ADR 0001.

## Context

The specification requires that `make toolchain-check` check every dependency
with clear, macOS-specific hints and without installing anything, that critical
external dependencies be pinned, and that `make clean && make` not depend on
leftovers of a previous build.

`make toolchain-check` checked presence only. It missed `clang`, which
assembles `kernel/arch/amd64/io.S` and `userspace/runtime/amd64.S`, gave a hint
only for the firmware, and compared no versions. Nothing recorded which Go and
TinyGo a build was known to work with, although together they compile every
byte of the kernel and userspace.

Nobody had compared two clean builds. Doing so showed the compiled artifacts —
kernel, every userspace ELF and native image, the command index, and
`initramfs.tar` — already byte-identical. The EFI system partition and the ISO
were not. `hdiutil create` stamps every FAT entry with the build time, picks a
random volume serial number, adds AppleDouble `._` files, and leaves deleted
entries with random temporary names; `hdiutil makehybrid` stamps the volume
descriptors and directory records. Every build produced a different ISO.

## Decision

**One file pins the toolchain.** `tools/toolchain.versions` lists each external
tool, its known-good version, and what a mismatch means. Go 1.25.5 and TinyGo
0.42.0 are `error` pins: they produce the binaries, so another version fails
the check. Apple clang 21.0.0, GNU Make 3.81, QEMU 11.0.1, and the MacPorts
OVMF image (by SHA-256) are `warn` pins: clang only assembles two hand-written
files whose encodings the source fixes, make only orders the steps, and QEMU
and the firmware only run the result. A difference there is reported and
accepted, and `make test-reproducible` would show if it changed an artifact.
The check also requires the pinned Go to match the `go` directive of `go.mod`.

**The check reports and never installs.** Each tool is `[ OK ]`, `[MISS]`,
`[FAIL]` (an `error` pin differs), or `[WARN]`, in the style of the boot log.
Every problem carries a concrete macOS hint: the go.dev package and the TinyGo
release archive for the pinned versions, `xcode-select --install` for clang,
make, and nm, and MacPorts and Homebrew commands for QEMU. A firmware found at
another known location is named with the `OVMF_CODE=` to pass. When the Xcode
Command Line Tools are absent the check does not run the `/usr/bin` stubs,
because they offer to install them. Missing tools and `error` mismatches exit
non-zero. The build runs Go with `GOTOOLCHAIN=local`, so the go command never
fetches a toolchain other than the one pinned.

**`tools/mkiso` writes the boot images.** It builds the 8 MiB FAT16 EFI system
partition (`EFI/BOOT/BOOTX64.EFI` and `initramfs.tar`, which needs a long name)
and the ISO 9660 volume with a single El Torito entry for that partition under
the UEFI platform ID. Entries are in name order, the serial number is fixed,
and every timestamp is `SOURCE_DATE_EPOCH` (0 by default, which FAT clamps to
1980). The image is the FAT volume itself, so the `dd` that stripped hdiutil's
MBR wrapper and the catalog patch of `tools/iso-uefi` are gone. `hdiutil` is no
longer a build dependency.

**`make test-reproducible` proves it.** `tests/build/reproducible.sh` cleans
`build/` (keeping disks, logs, and reports), builds the ISO, copies the
outputs to `build/reproducible/first`, cleans again, including the Go and
TinyGo caches, builds again, and compares SHA-256 of every artifact. It names
each file that differs and fails; on success it prints the hashes of the ISO,
the EFI partition, the kernel, `initramfs.tar`, and the init and shell images.
Both builds run in the ordinary `build/` because the TinyGo target files name
`build/kernel/io.o` and `build/userspace/runtime.o`.

## Alternatives

**Normalising hdiutil's output.** Rewriting timestamps and the serial number
after the fact still leaves the AppleDouble files and the deleted entries with
random names, whose presence depends on the host's extended attributes. A
generator whose every byte is chosen is simpler to trust than a patcher of a
format it does not control.

**xorriso and mtools.** They produce reproducible images, but they are further
downloaded dependencies to replace a few hundred lines of Go that the tests
exercise directly.

**Every pin an error.** Failing on a newer QEMU or a different Apple clang would
block building on a machine whose tools are fine, while the reproducibility
check already detects the case where a warning-level tool changes an artifact.

**Building into two separate output directories.** It would also show that no
output path is embedded, but the TinyGo targets fix two object paths under
`build/`; a clean-and-rebuild in place is exactly the `make clean && make` the
specification names.

## Consequences

Two clean builds are byte-identical at the default configuration: 66 artifacts,
including `build/goos-x86_64.iso`, compare equal. `fsck_msdos` accepts the EFI
partition, macOS mounts both images, and `make test-boot` passes on the new ISO.

The pins must move deliberately: changing `tools/toolchain.versions` goes with
a clean `make test-reproducible` and `make test-all` on the new version. A
contributor with another Go or TinyGo must install the pinned one before
`make toolchain-check` passes; the build itself does not refuse to run.

The ISO 9660 tree no longer has Joliet names, so macOS shows `INITRAMFS.TAR`
and `EFI.IMG` in upper case when the ISO is mounted. Firmware boots from the
El Torito partition, whose FAT long names are unchanged.

`make test-reproducible` removes the build outputs and caches under `build/`,
as `make clean` does, so the next build starts cold.
