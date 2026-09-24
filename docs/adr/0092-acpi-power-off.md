# ADR 0092: Power off through ACPI S5

## Status

Accepted. Extends ADR 0061.

## Context

The shell could restart the machine but not switch it off. `reboot` syncs
storage through PID 1 and then resets the machine through the chipset (ADR
0061). Stopping GoOS otherwise meant killing QEMU, so the persistent volume was
never flushed.

A PC switches off through ACPI. The firmware's tables name the PM1a control
register, and the DSDT names the sleep type for S5, the soft-off state. Writing
that type with the sleep-enable bit set turns the machine off. The kernel read
no ACPI at all, and the specification's boot information (§5.3) lists the ACPI
root pointer as something the kernel should receive.

## Decision

**The boot code records the RSDP.** Before `ExitBootServices`,
`FindACPIRoot` looks for the ACPI 2.0 configuration table in the UEFI system
table, falling back to ACPI 1.0, and stores its address in `boot.Info.ACPIRoot`.
It scans at most 64 entries.

**The kernel reads the tables once, at boot, as untrusted input.** Package
`kernel/acpi` follows the RSDP to the XSDT, or to the RSDT for ACPI 1.0, then
to the FADT and the DSDT. It verifies each table's signature, length and
checksum before reading any field from it. A table's length is bounded to
64 KiB, the root table to 64 entries, and reads to the identity-mapped first
GiB, so a firmware pointer outside it is refused rather than dereferenced. From
the FADT it takes the PM1a control port, preferring the 64-bit field when that
field names I/O space. From the DSDT's AML it takes the first element of the
`\_S5_` package. Only the encodings firmware uses for a small integer are
accepted; anything else is refused rather than guessed. The kernel reads the
tables straight after leaving boot services, while nothing has reused memory,
and keeps only the port and the value. A machine without usable tables boots
normally and cannot power off.

**`SystemPowerOff` (native call 36) writes the value.** Like `SystemReset`, it
needs the system-control capability, which only PID 1 holds, and it does not
return when it works. It answers `Unsupported` when the tables were not usable.

**`poweroff` goes the way `reboot` does.** The shell sends `ControlPowerOff`,
exec control protocol version 4. PID 1 has the filesystem service sync
storage, prints `Powering off...`, and makes the call. If the sync fails,
nothing is switched off and the shell reports why.

## Alternatives

**UEFI runtime services' `ResetSystem(EfiResetShutdown)`.** This needs the
firmware's runtime code mapped executable in the kernel's page table and a
call into firmware from the running kernel. That is more firmware code trusted
at run time than reading two tables once at boot.

**Hard-coding QEMU's port and value.** q35 puts PM1a control at `0x604`. A
constant would work on one machine model and silently do the wrong thing on
another, and the specification keeps real hardware open.

**QEMU's `isa-debug-exit` device.** It ends QEMU with an exit code, which is
useful for tests, but it is not how a machine powers off, and it is not
present unless QEMU is started with it.

## Consequences

`poweroff` flushes the persistent volume and ends QEMU. The boot test writes a
file, powers off, requires QEMU to exit by itself, and reads the file back on
a second boot.

The kernel now parses firmware data. The parser is host-tested against valid
tables and against broken checksums, signatures, lengths, missing tables and
malformed `\_S5_` encodings, and it keeps no pointer into firmware memory after
boot.
