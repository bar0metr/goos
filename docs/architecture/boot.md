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
- `initramfs.tar`: a USTAR archive with `system/init`, the storage
  services, the terminal and the shell, the recovery commands in
  `system/bin/`, and their index `system/commands`, cut from the index the
  build generates from the commands' manifests (`tools/initramfs`,
  `tools/commands`, ADR 0080, ADR 0175).

The rest of the system is installed on the disk: `make run` boots
`build/goos-disk.img`, made from the build's system tree when it does not
exist and never changed when it does (ADR 0175).

The ESP is the ISO's El Torito boot image, an 8 MiB FAT volume
(`tools/mkiso`, ADR 0086). QEMU runs q35 with OVMF, 512 MiB RAM (`MEMORY`), COM1 on
stdio, and a legacy virtio-blk disk (`Makefile`, `QEMU_COMMON_FLAGS`). `make
run` adds a headless display, while `make run-gui` exposes the emulated VGA
device and GOP framebuffer in a host window. The manual target defaults to the
Cocoa backend and accepts `QEMU_DISPLAY` overrides; headless targets keep
`-display none` and never load a GUI backend.

Nothing in this chain is signed or verified. How firmware, kernel, system
image, and services would be verified, with keys the machine owner controls,
is described in [boot-chain.md](../security/boot-chain.md).

## Firmware phase

1. **Serial.** COM1 is initialized first so every later failure can be
   reported.
2. **Initramfs load.** Using the loaded-image and simple-file-system
   protocols, the kernel opens `\initramfs.tar` on its boot volume and reads it
   into at most `boot.MaxInitramfs` (6 MiB) of `EfiLoaderData`
   (`kernel/boot/uefi/initramfs.go`, ADR 0020). A larger archive stops the boot
   with `PANIC: initramfs is larger than the 6 MiB the loader reads` instead of
   being used cut short, and `tools/initramfs` refuses to build one. The pages
   past the archive go back to the firmware.
3. **Graphics discovery.** The kernel locates GOP, queries at most 256 modes,
   chooses a directly writable 32-bit mode by the resolution policy in ADR
   0123, and copies only validated address, size, geometry, stride, pixel
   format, and masks into `boot.Info`. Invalid or absent graphics prints a
   warning and leaves the boot serial-only. No GOP pointer or firmware-owned
   mode record crosses the boot boundary.
4. **Memory map and exit.** The kernel captures the final UEFI memory map into
   a fixed buffer, normalizes region types, merges adjacent regions of one
   type into at most 256 regions, and calls `ExitBootServices` with
   the matching key, retrying on a stale key without allocating in between
   (ADR 0002, [memory.md](memory.md)). No firmware service is called after
   this point. Just before, the kernel records the ACPI RSDP from the UEFI
   configuration table. Right after, it reads the FADT and DSDT once, checking
   every checksum and length, and keeps only the port and value that switch the
   machine off (`kernel/acpi`, ADR 0092). The same checked root-table walker
   reads the MADT processor entries. CPUID identity, topology, cache, and
   feature leaves are combined with that bounded firmware list into an
   immutable CPU inventory (ADR 0180). The bootstrap processor later starts
   the others, which join the scheduler ([SMP](../kernel/smp.md), ADR 0181).

Fatal UEFI handoff failures print a panic line and halt. Optional hardware
discovery, including CPU inventory, degrades conservatively when its inputs
are absent or inconsistent.

## Kernel bring-up

| Step | What happens | Reference |
| --- | --- | --- |
| PCI discovery | Find the required legacy virtio-blk function and the optional legacy virtio-net function independently; validate their BAR/IRQ routing and enable only the decoded I/O and bus-mastering state each driver needs | ADR 0034, ADR 0190 |
| PMM | Build one bitmap segment per run of conventional memory, carving the bitmaps from low memory; allocate and free one page as a self-test | ADR 0002, 0103 |
| VM | Require NX and PAT; build the kernel identity map with PE section permissions and the direct map of managed memory; map loader memory above 1 GiB; program a known PAT layout; map the validated GOP framebuffer in its kernel-only UC window; enable `EFER.NXE` and `CR0.WP`; load CR3; fill the screen with the framebuffer milestone pattern | ADR 0003, 0103, 0123 |
| GDT, IDT, TSS | Install descriptor tables and the user native-call gate; map LAPIC/IOAPIC; route GSI 11 to virtio, GSI 4 to UART, and GSI 1 to PS/2; enable UART receive and perform bounded i8042 initialization; test a breakpoint trap (validation build) | ADR 0004, 0035, 0075, 0123 |
| Timer | Program the PIT at the build-time rate (`HZ`); wait for three ticks; calibrate the TSC over about 100 ms | ADR 0005, 0052, 0054 |
| Scheduler | Allocate guarded kernel stacks; reset per-slot FXSAVE areas; start kernel tasks: housekeeping (slot 0), two demo tasks that sleep at once in a normal build, idle | ADR 0006, 0055, 0056, 0058 |

## First processes

The kernel creates **PID 1** before ordinary userspace starts. PID 1 is loaded
from `system/init` in the boot archive and receives the system resource
profile (ADR 0072, ADR 0175).

Its fixed initial handle table contains the lifecycle/control endpoints,
console devices, VFS/exec/block bootstrap channels, storage device authority,
system-control and logging authority, launch staging/shared objects, the
boot-archive image capability, the non-transferable loader capability, and
the optional network device authority. The exact checked slot layout is in
[capabilities.md](../security/capabilities.md#holders-at-boot).

A validation build (`VALIDATION=1`, ADR 0058) additionally runs kernel
self-tests and creates the built-in ABI/fault processes used by the boot
harness. Those processes and validation markers are not part of a normal
system.

Before PID 1 starts its service policy, the bootstrap processor brings the
other admitted processors online. From then on the kernel services lifecycle
requests, exits, deadlines and housekeeping while scheduler state is shared
across the online CPUs (ADR 0181, ADR 0184).

## PID 1 and service start order

PID 1 is a freestanding TinyGo program (ADR 0023). It maps the read-only boot
archive through its image capability. Programs under `/bootstrap` use
archive extents; installed/user programs are read through VFS into sealed
file-backed executable sources (ADR 0093, ADR 0121, ADR 0175).

The current startup is:

1. In a validation build, run the launch/stream/bootstrap probes and adopt the
   built-in fault process used by the supervisor tests.
2. Start the archive-backed storage chain: VFS, block, virtio-blk and FAT32.
   VFS exposes the archive at `/bootstrap`, attaches the volume at
   `/persist`, `/home` and `/state`, and marks its `system` directory
   protected.
3. Select normal or recovery mode (ADR 0175). Normal mode requires a compatible
   `/persist/system/release`, acceptable session-service/program headers and
   command index, then publishes `/system` and `/apps`. Recovery keeps the
   installed system unpublished.
4. In normal mode start configuration and logging, then networking
   (`netd` and the optional `virtio-net` driver), `linuxsockd`, protect
   remote state and start `remoted`. These services are installed-system
   file-backed executables. Their failures are contained/restarted according to
   their supervisor policy.
5. For every local/remote terminal session, start the archive-backed terminal
   service and then the shell: `/system/services/shell` in normal mode or
   `/bootstrap/shell` in recovery. Remote SSH workers and per-session
   `sftpd` processes are created on demand with narrower authority.
6. In validation builds, run the configured fault injections only after the
   system they exercise is live.
7. Enter the event loop. One bounded `WaitAny` set covers lifecycle traffic,
   filesystem replacement notices, shell/remote exec channels, child-process
   completion, file-fill/source operations, deferred launches and remote
   worker/control events; deadlines bound restart/shutdown work.

A launch request names either an extent of the boot archive or a whole sealed
executable source. The kernel checks Execute authority, source bounds/state,
the native or Linux image format, the capability manifest and process resource
profile before it publishes the new process. Static Linux PIE placement is
chosen during this preparation (ADR 0219), before the new address space becomes
visible.

Storage-service restart is special because normal-mode programs are backed by
that storage. PID 1 withholds/revalidates installed-system services while the
backend is unavailable and enters recovery when the normal system can no
longer be maintained (ADR 0175).

## Build variants

| Variable | Values | Effect |
| --- | --- | --- |
| `HZ` | 100, 250 (default), 1000 | build tag `goos_hz<rate>`; selects the PIT divisor and every userspace deadline (ADR 0052) |
| `DIAG` | 0 (default), 1 | build tag `goos_diag`; enables the kernel event ring, the `Diagnostics` operation, and launch-stage timing (ADR 0052) |
| `VALIDATION` | 0 (default), 1 | build tag `goos_validation`; runs the kernel self-tests, processes 2 and 3, the supervision scenario, the storage probe, and the driver fault, and prints success markers (ADR 0058) |
| `RECOVERY` | 0 (default), 1 | `run`, `run-gui`, `run-debug`: boot an ISO whose archive asks PID 1 for recovery mode (ADR 0175) |

`build/config/build` records the active values. Each combination builds into
its own tree under `build/variants/`, and `build/goos-x86_64.iso` is the most
recently requested one. `make test-boot` always builds the validation ISO; a
normal build prints a short banner, `[ OK ]` per service, and the prompt.

The kernel's built-in checks, the demo processes, the fault-probe service, and
the success markers printed along the way are validation, not system function.
They run only in the `VALIDATION=1` build (ADR 0058); a normal boot prints a
short status summary and reaches the shell prompt.
