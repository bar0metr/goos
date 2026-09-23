# ADR 0087: Bounded boot archive

## Status

Accepted. Amends ADR 0020.

## Context

The kernel read `initramfs.tar` into a fixed buffer of 512 pages. The firmware
read is bounded by the buffer, and the kernel never compared what it read with
the file, so an archive over 2 MiB would boot with its tail silently missing:
whatever executables or files the tar held last would not exist. Nothing at
build time checked the size either.

Commands became separate images in ADR 0080, and the archive grew to 1.6 MiB,
four fifths of the buffer. A few more commands would have crossed it.

## Decision

**One bound, shared by the build and the kernel.** `boot.MaxInitramfs` is 4 MiB.
`tools/initramfs` refuses to write a larger archive, and the kernel allocates
its read buffer from the same constant.

**An archive over the bound stops the boot.** A read that fills the buffer is
followed by a one-byte read; if that returns data, the kernel prints
`PANIC: initramfs is larger than the 4 MiB the loader reads` and halts instead
of running a truncated system image.

**The unused part of the buffer goes back.** Pages past the archive are freed to
the firmware before `ExitBootServices`, so a larger bound costs no memory.

## Alternatives

**Reading the file size first.** `EFI_FILE_INFO` gives the size, so the buffer
could fit the file exactly. It adds a protocol call and a variable-size
structure to parse for what one extra read already answers.

**Embedding the archive in the kernel image.** It removes the separate read and
is one of the options the boot chain keeps open for signing (boot-chain.md). It
changes how the ISO is built and is not needed to stop truncation.

## Consequences

The 8 MiB boot partition holds the kernel and an archive at the bound with room
to spare. The build test covers the size check; the refusal path was checked by
booting an ISO built by hand around a 4.5 MiB archive, which halts with the
message above.
