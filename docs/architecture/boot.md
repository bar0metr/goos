# Boot

This document follows `main` in `kernel/main.go` from firmware entry to the
steady-state kernel loop, then PID 1's service startup in
`userspace/cmd/init/main.go`. It describes order and mechanism. Exact serial
output is deliberately not listed: boot output is being reworked (see
[Build variants](#build-variants)).

## Artifacts

`make iso` produces `build/goos-x86_64.iso` with an EFI system partition that
contains:

- `EFI/BOOT/BOOTX64.EFI`: the kernel, a TinyGo UEFI PE image linked at
  `0x400000` (ADR 0001);
- `initramfs.tar`: a USTAR archive with `system/init`, the services, one
  image per command in `system/bin/`, and the command index
  `system/commands` generated from the commands' manifests
  (`tools/initramfs`, `tools/commands`, ADR 0080).

QEMU runs q35 with OVMF, 512 MiB RAM, COM1 on stdio, and a legacy virtio-blk
disk (`Makefile`, `QEMU_FLAGS`).

## Firmware phase

1. **Serial.** COM1 is initialized first so every later failure can be
   reported.
2. **Initramfs load.** Using the loaded-image and simple-file-system
   protocols, the kernel opens `\initramfs.tar` on its boot volume and reads it
   into 512 pages (2 MiB) of `EfiLoaderData` (`kernel/boot/uefi/initramfs.go`,
   ADR 0020).
3. **Memory map and exit.** The kernel captures the final UEFI memory map into
   a fixed buffer, normalizes region types, and calls `ExitBootServices` with
   the matching key, retrying on a stale key without allocating in between
   (ADR 0002, [memory.md](memory.md)). No firmware service is called after
   this point.

Any failure in this phase prints a panic line and halts.

## Kernel bring-up

| Step | What happens | Reference |
| --- | --- | --- |
| PCI discovery | Find the legacy virtio-blk function through configuration mechanism 1; require IRQ line 11; enable I/O decoding, bus mastering, and INTx | ADR 0034 |
| PMM | Build the physical bitmap from conventional memory below 1 GiB; allocate and free one page as a self-test | ADR 0002 |
| VM | Require NX; build the kernel identity map with PE section permissions; enable `EFER.NXE` and `CR0.WP`; load CR3 | ADR 0003 |
| GDT, IDT, TSS | Install descriptor tables and the user native-call gate; map LAPIC/IOAPIC; route GSI 11 to the virtio vector; test a breakpoint trap | ADR 0004, 0035 |
| Timer | Program the PIT at the build-time rate (`HZ`); wait for three ticks; calibrate the TSC over about 100 ms | ADR 0005, 0052, 0054 |
| Scheduler | Allocate guarded kernel stacks; reset per-slot FXSAVE areas; start kernel tasks: housekeeping (slot 0), two demo tasks, idle | ADR 0006, 0055, 0056 |

## First processes

The kernel creates three processes before any user code runs:

- **PID 1**, loaded from `system/init` in the initramfs;
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

The three user tasks are added to the scheduler with interrupts disabled.
From then on the boot thread runs `serviceKernel` in a loop: each step disables
interrupts, expires wait deadlines, retries retained lifecycle messages,
delivers task exits, serves at most one launch request from PID 1, and sleeps
until the next tick, exit, or request (ADR 0053, 0055). The loop first waits
until PID 1 has completed its bootstrap, then continues forever.

## PID 1 and service start order

PID 1 is a freestanding TinyGo program (ADR 0023). It:

1. probes a native byte stream end to end;
2. registers `fault-probe` (restart on failure) and adopts the already running
   PID 3 as its first instance;
3. handles each fault-probe exit through the supervisor, which relaunches
   `/system/fault-probe` until the restart allowance is exhausted and the
   service is marked failed (ADR 0019, 0021);
4. registers and starts, in this order and with `RestartNever`:
   - `vfs`, the VFS service with the RAMFS root and the FAT32 mount at
     `/persist`;
   - `block`, the block service;
   - `virtio-blk`, the userspace driver;
   - `shell`, which depends on `vfs`;
5. probes the block service with an info, write, read, and flush exchange;
6. enters its main loop: one `WaitAny` over the lifecycle channel and the
   shell's exec channel (ADR 0051).

Each launch is a correlated request on the lifecycle channel. The kernel
validates the capability manifest, loads the image, installs handles, binds
the startup block, and creates the task (ADR 0022, 0024, 0031). Steps 2 and 3
exist to validate supervision and fault isolation during every boot.

## Build variants

| Variable | Values | Effect |
| --- | --- | --- |
| `HZ` | 100 (default), 250, 1000 | build tag `goos_hz<rate>`; selects the PIT divisor and every userspace deadline (ADR 0052) |
| `DIAG` | 0 (default), 1 | build tag `goos_diag`; enables the kernel event ring, the `Diagnostics` operation, and launch-stage timing (ADR 0052) |

`build/config/build` records the active values, and changing either rebuilds
kernel and userspace.

The kernel's built-in checks, the demo processes, the fault-probe service, and
the success markers printed along the way are validation, not system function.
They run only in the `VALIDATION=1` build (ADR 0058); a normal boot prints a
short status summary and reaches the shell prompt.
