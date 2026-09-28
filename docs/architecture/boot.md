# Boot

This document follows `main` in `kernel/main.go` from firmware entry to the
steady-state kernel loop, then PID 1's service startup in
`userspace/cmd/init/main.go` and `services.go`. It describes order and mechanism. Exact serial
output is deliberately not listed; it differs between the normal and the
validation build (see [Build variants](#build-variants)).

## Artifacts

`make iso` produces `build/goos-x86_64.iso` with an EFI system partition that
contains:

- `EFI/BOOT/BOOTX64.EFI`: the kernel, a TinyGo UEFI PE image linked at
  `0x400000` (ADR 0001);
- `initramfs.tar`: a USTAR archive with `system/init`, the services, one
  image per command in `system/bin/`, and the command index
  `system/commands` generated from the commands' manifests
  (`tools/initramfs`, `tools/commands`, ADR 0080).

The ESP is the ISO's El Torito boot image, an 8 MiB FAT volume
(`tools/mkiso`, ADR 0086). QEMU runs q35 with OVMF, 512 MiB RAM (`MEMORY`), COM1 on
stdio, and a legacy virtio-blk disk (`Makefile`, `QEMU_FLAGS`).

Nothing in this chain is signed or verified. How firmware, kernel, system
image, and services would be verified, with keys the machine owner controls,
is described in [boot-chain.md](../security/boot-chain.md).

## Firmware phase

1. **Serial.** COM1 is initialized first so every later failure can be
   reported.
2. **Initramfs load.** Using the loaded-image and simple-file-system
   protocols, the kernel opens `\initramfs.tar` on its boot volume and reads it
   into at most `boot.MaxInitramfs` (4 MiB) of `EfiLoaderData`
   (`kernel/boot/uefi/initramfs.go`, ADR 0020). A larger archive stops the boot
   with `PANIC: initramfs is larger than the 4 MiB the loader reads` instead of
   being used cut short, and `tools/initramfs` refuses to build one. The pages
   past the archive go back to the firmware.
3. **Graphics discovery.** The kernel locates GOP, queries at most 256 modes,
   chooses a directly writable 32-bit mode by the resolution policy in ADR
   0123, and copies only validated address, size, geometry, stride, pixel
   format, and masks into `boot.Info`. Invalid or absent graphics prints a
   warning and leaves the boot serial-only. No GOP pointer or firmware-owned
   mode record crosses the boot boundary.
4. **Memory map and exit.** The kernel captures the final UEFI memory map into
   a fixed buffer, normalizes region types, and calls `ExitBootServices` with
   the matching key, retrying on a stale key without allocating in between
   (ADR 0002, [memory.md](memory.md)). No firmware service is called after
   this point. Just before, the kernel records the ACPI RSDP from the UEFI
   configuration table. Right after, it reads the FADT and DSDT once, checking
   every checksum and length, and keeps only the port and value that switch the
   machine off (`kernel/acpi`, ADR 0092).

Any failure in this phase prints a panic line and halts.

## Kernel bring-up

| Step | What happens | Reference |
| --- | --- | --- |
| PCI discovery | Find the legacy virtio-blk function through configuration mechanism 1; require IRQ line 11; enable I/O decoding, bus mastering, and INTx | ADR 0034 |
| PMM | Build one bitmap segment per run of conventional memory, carving the bitmaps from low memory; allocate and free one page as a self-test | ADR 0002, 0103 |
| VM | Require NX and PAT; build the kernel identity map with PE section permissions and the direct map of managed memory; map loader memory above 1 GiB; program a known PAT layout; map the validated GOP framebuffer in its kernel-only UC window; enable `EFER.NXE` and `CR0.WP`; load CR3; fill the screen with the framebuffer milestone pattern | ADR 0003, 0103, 0123 |
| GDT, IDT, TSS | Install descriptor tables and the user native-call gate; map LAPIC/IOAPIC; route GSI 11 to the virtio vector and GSI 4 to the console vector, then enable the UART receive interrupt; test a breakpoint trap (validation build) | ADR 0004, 0035, 0075 |
| Timer | Program the PIT at the build-time rate (`HZ`); wait for three ticks; calibrate the TSC over about 100 ms | ADR 0005, 0052, 0054 |
| Scheduler | Allocate guarded kernel stacks; reset per-slot FXSAVE areas; start kernel tasks: housekeeping (slot 0), two demo tasks that sleep at once in a normal build, idle | ADR 0006, 0055, 0056, 0058 |

## First processes

In a normal build the kernel creates one process before any user code runs:
**PID 1**, loaded from `system/init` in the initramfs, with the system quota
(ADR 0072). Services start at PID 4.

PID 1's handle table is filled in a fixed order with the lifecycle channel,
the console device, the service channel endpoints, the virtio port, IRQ, and
DMA authorities, the system-control capability, the launch staging and exec
pages, and the system image capability. The kernel panics if any slot lands
out of order. The table is listed in
[docs/security/capabilities.md](../security/capabilities.md#holders-at-boot).

A validation build (`VALIDATION=1`, ADR 0058) also runs the kernel's built-in
checks of handle transfer, typed IPC, RAMFS, VFS, shared memory, shared image
pages, quotas, address-space isolation, and user-copy validation, which halt
the boot on failure, and creates two more processes:

- **PID 2**, a small in-kernel native program that exercises the native-call
  ABI and IPC;
- **PID 3**, an in-kernel program that deliberately faults, used as the
  first supervised service.

PID 1's handle table is filled in a fixed order with the lifecycle channel,
the console device, the service channel endpoints, and the virtio port, IRQ,
and DMA authorities. The table is listed in
[docs/security/capabilities.md](../security/capabilities.md#holders-at-boot).

In a validation build (ADR 0058) the kernel runs built-in checks of handle
transfer, typed IPC, RAMFS, VFS, shared memory, address-space isolation, and
user-copy validation between these steps. They halt the boot on failure. The
RAMFS and VFS checks are compiled only into that build, so the normal kernel
links no filesystem code (ADR 0084).

The user tasks are added to the scheduler with interrupts disabled. From then
on the boot thread runs `serviceKernel` in a loop: each step disables
interrupts, expires wait deadlines, retries retained lifecycle messages,
delivers task exits, serves at most one launch request from PID 1, and sleeps
until the next tick, exit, or request (ADR 0053, 0055). A validation build
first waits until PID 1 has completed its bootstrap. If PID 1 exits, the
kernel halts.

## PID 1 and service start order

PID 1 is a freestanding TinyGo program (ADR 0023). It first maps the
read-only system image through its image capability: every service and every
command in `/system/bin` it launches is found there (ADR 0093); a program
elsewhere is read into an executable object (ADR 0097). In a validation build
it then:

1. proves that the kernel refuses launches without Execute authority, outside
   the image, or of bytes that are not a native image, and executable objects
   that are unsealed, named in part, closed, or not a program
   (`LAUNCH_AUTHORITY_OK`),
   and probes a native byte stream end to end;
2. registers `fault-probe` (restart on failure) and adopts the already running
   PID 3 as its first instance;
3. handles each fault-probe exit through the supervisor, which relaunches
   `/system/fault-probe` until the restart allowance is exhausted and the
   service is marked failed (ADR 0019, 0021).

In every build it then:

1. maps the exec area and creates the block service's control channel and the
   channel between the VFS and the FAT32 service;
2. registers and starts `vfs` (VFS with the RAMFS root and the read-only system
   image at `/system`), restarted on failure with its clients handed new
   sessions (ADR 0099), and `block`, `virtio-blk` and `fat32`, all restarted on
   failure, the driver and `fat32` after `block` (ADR 0101); once `fat32` runs, hands the VFS its
   channel to it and waits for the volume to be mounted at `/persist`, `/home`,
   and `/state` (ADR 0070, 0071, 0078, 0096);
3. in a validation build, probes the block service with an info, write, read,
   and flush exchange;
4. starts `config` and then `log`, both restarted on failure; a failure to
   start either is a warning, not a stopped boot (ADR 0073, 0074);
5. creates the terminal's output stream, session and control channels and
   starts `tty`, then opens a full-scope configuration session for the shell
   and starts `shell`, which depends on `vfs` and `tty` (ADR 0076, 0082).
   Both are `RestartAlways` and restart independently on a renewed session
   (ADR 0089);
6. in a validation build, kills the driver once to exercise its restart
   (ADR 0078);
7. enters its main loop: one `WaitAny` over the lifecycle channel, the shell's
   exec channel, and the next process of each running job, bounded by the
   restart backoff while a restart is pending (ADR 0051, 0064, 0078).

Each launch is a correlated request on the lifecycle channel that names the
executable as an extent of the system image, or a sealed executable object.
The kernel checks Execute on the image capability and the extent's bounds, or
Execute on the object and that it is sealed and named whole, validates the
capability manifest,
loads the image, installs handles, binds the startup object, and creates the
task (ADR 0024, 0031, 0062, 0093).

## Build variants

| Variable | Values | Effect |
| --- | --- | --- |
| `HZ` | 100 (default), 250, 1000 | build tag `goos_hz<rate>`; selects the PIT divisor and every userspace deadline (ADR 0052) |
| `DIAG` | 0 (default), 1 | build tag `goos_diag`; enables the kernel event ring, the `Diagnostics` operation, and launch-stage timing (ADR 0052) |
| `VALIDATION` | 0 (default), 1 | build tag `goos_validation`; runs the kernel self-tests, processes 2 and 3, the supervision scenario, the storage probe, and the driver fault, and prints success markers (ADR 0058) |

`build/config/build` records the active values. Each combination builds into
its own tree under `build/variants/`, and `build/goos-x86_64.iso` is the most
recently requested one. `make test-boot` always builds the validation ISO; a
normal build prints a short banner, `[ OK ]` per service, and the prompt.

The kernel's built-in checks, the demo processes, the fault-probe service, and
the success markers printed along the way are validation, not system function.
They run only in the `VALIDATION=1` build (ADR 0058); a normal boot prints a
short status summary and reaches the shell prompt.
