# ADR 0001: TinyGo UEFI bootstrap

Status: Accepted

## Context

The first supported host is macOS arm64 and the first guest is QEMU x86_64.
TinyGo 0.42 ships a maintained `uefi-amd64` target. A separate bootloader would
add a downloaded, versioned dependency before the kernel boundary exists.

## Decision

Milestone 0 uses TinyGo's UEFI target as the early boot environment. The EFI
application is installed as `EFI/BOOT/BOOTX64.EFI` in an El Torito EFI image.
It writes directly to COM1 so boot tests do not depend on graphical firmware
output. Firmware boot services are an early-boot mechanism, not a permanent
kernel API. The memory milestone must capture the UEFI memory map and call
ExitBootServices before enabling the kernel allocator and scheduler.

macOS `hdiutil` emits an El Torito catalog with the legacy BIOS platform ID.
The checked-in `tools/iso-uefi` validator changes it to the standardized EFI
platform ID, recomputes the validation checksum, and records the full FAT image
size. Malformed input is rejected rather than patched heuristically.
`hdiutil create` emits an MBR-wrapped FAT volume; the ISO recipe removes that
single-sector wrapper because the UEFI El Torito format requires the FAT volume
itself as the no-emulation boot image.

## Alternatives

Limine provides a convenient boot protocol but introduces a pinned external
binary dependency and ISO tooling. A hand-written UEFI loader duplicates code
already maintained by TinyGo. Multiboot through legacy BIOS conflicts with the
preferred modern boot path.

## Consequences

Milestone 0 remains dependency-light and boots on QEMU/OVMF. The kernel is PE
rather than ELF until a loader boundary is introduced. `goos.elf`, linker maps,
and post-ExitBootServices ownership remain work for Milestone 1; the artifact
is therefore named `goos.efi` and documentation states the limitation.
