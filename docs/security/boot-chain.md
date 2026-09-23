# Boot chain and signature verification

**Status: not implemented.** Nothing in GoOS verifies a signature, a hash, or
a measurement today. Whoever can change the ISO controls the system. This
document describes the chain as it is, where each verification step would go,
and what already exists to support it. It answers spec §40: the architecture
must provide for UEFI → bootloader → kernel → system image → services with
signature checks, the owner of the machine must be able to use their own keys,
and a mandatory vendor-controlled lock-in is forbidden.

The boot sequence itself is described in
[boot.md](../architecture/boot.md); the resulting trust assumptions are in
[threat-model.md](threat-model.md).

## The chain today

```text
OVMF (UEFI firmware, pflash, read-only)
 └─ El Torito EFI boot image: 8 MiB FAT volume on the ISO
     ├─ EFI/BOOT/BOOTX64.EFI       kernel, PE32+, linked at 0x400000   ── loaded and started by firmware
     └─ initramfs.tar              system image, USTAR                 ── read by the kernel through UEFI
          ├─ system/init           PID 1                               ── loaded by the kernel
          ├─ system/vfs, block, virtio-blk, config, log, shell         ── launched by PID 1 through the kernel
          ├─ system/bin/<command>  one image per command               ── launched on request from the shell
          ├─ system/commands       command index: grants per command   ── read by PID 1 through VFS
          └─ system/motd, system/fault-probe (launched only by the validation build)
```

1. **Firmware.** QEMU runs OVMF from `edk2-x86_64-code.fd` as read-only
   pflash (`Makefile`, `QEMU_FLAGS`). No variable store is attached, so no
   Secure Boot keys can be enrolled and Secure Boot is not in effect.
2. **ISO.** `make iso` copies the kernel to `EFI/BOOT/BOOTX64.EFI` and the
   archive to `initramfs.tar`, builds an 8 MiB FAT volume from them with
   `hdiutil`, removes the MBR wrapper `hdiutil` adds, and uses the result as
   the no-emulation El Torito boot image. `tools/iso-uefi` then rewrites the
   boot catalog entry to the EFI platform ID, recomputes its checksum, and
   records the image size (ADR 0001). The tool validates the catalog it
   patches; it adds no signature.
3. **Kernel.** The firmware finds the EFI El Torito entry, mounts the FAT
   volume, and starts the removable-media default `\EFI\BOOT\BOOTX64.EFI`.
   There is no separate bootloader: the kernel is a TinyGo UEFI application
   and is its own loader. It is a PE image linked at `0x400000` without
   `DYNAMIC_BASE`, and the kernel reads its own header at that address
   (`vm.LoadedImage`).
4. **System image.** Before `ExitBootServices`, the kernel asks the
   loaded-image protocol for the device it was loaded from, opens
   `\initramfs.tar` on that volume through the simple-file-system protocol,
   and reads it into at most `boot.MaxInitramfs` (4 MiB) of `EfiLoaderData`
   below 1 GiB (`kernel/boot/uefi/initramfs.go`, ADR 0020). A read that fills
   the buffer is followed by a one-byte read; an archive over the bound stops
   the boot rather than being used cut short, and the build refuses to produce
   one. The pages stay reserved after the handoff and are never writable from
   userspace.
5. **PID 1.** The kernel finds `system/init` in the archive, loads it through
   the native image loader, and runs it with the fixed capability layout.
   USTAR header checksums are verified on every lookup, which catches damage,
   not tampering.
6. **Services and commands.** PID 1 asks the kernel to launch
   `/system/<service>` and `/system/bin/<command>`. The kernel resolves every
   launch in the same archive with `initramfs.Locate`; there is no path by which
   a file on `/persist`, `/home`, `/state`, or `/tmp` can become a process
   (ADR 0022, 0030). The VFS service mounts the same archive read-only at
   `/system` and validates it as a whole at mount time (ADR 0070). PID 1 reads
   the command index `/system/commands` through VFS and grants a VFS session
   only to commands whose entry asks for one (ADR 0080).

## Where verification would be inserted

| Step | Verifier | Unit that is signed | Key store |
| --- | --- | --- | --- |
| firmware → kernel | UEFI Secure Boot in the firmware | the kernel EFI image `BOOTX64.EFI` (Authenticode signature in the PE) | UEFI `db`, `KEK`, `PK`, enrolled by the machine owner |
| kernel → system image | the kernel, before it parses the archive | the whole `initramfs.tar` | public keys the kernel trusts, owner-controlled (see below) |
| system image → services | nothing further | each image is a member of the verified archive | none needed |

**Kernel.** The kernel image is the first code GoOS controls, so UEFI Secure
Boot is the natural verifier and needs no GoOS code: the firmware refuses an
image whose signature does not chain to `db` or that `dbx` revokes. The build
would sign `goos.efi` after linking and before it is copied into the FAT
volume. With no separate bootloader there is no intermediate stage to sign; if
one is added later, it takes the kernel's place in this row and verifies the
kernel itself.

**System image.** The archive is read through firmware file services that
Secure Boot does not cover, so the kernel must verify it. The check belongs
right after `LoadInitramfs` returns and before the first `initramfs.Find`,
that is, before any byte of the archive is parsed. A failure stops the boot
with a console message, as a failed load does now. Two shapes fit the current
design, and the choice is open:

- a detached signature file beside `initramfs.tar` on the boot volume, read
  the same way, checked with a public key built into the kernel. The kernel's
  own Secure Boot signature then covers that key;
- the archive embedded as a section of the signed kernel PE, so one
  Authenticode signature covers both, and the kernel reads the archive from
  its own loaded image instead of from the file system.

The unit is the whole archive in both cases. Signing individual members would
need a manifest inside the archive and a per-launch check, and buys nothing
while the archive is immutable and fully resident.

**Services.** Once the archive is verified, no further check is needed for
the programs in it, because of properties the system already has:

- every executable the kernel will run comes from the archive, resolved by
  the kernel, not by a userspace component that could be compromised;
- the archive pages are never writable by a process: `ImageMap` maps them
  read-only for VFS, and the shared image frames built from them are never
  mapped writable (ADR 0070, 0081);
- the grants of each command come from the command index inside the same
  archive, so authority is covered by the same signature as code. PID 1 reads
  the index through VFS, so a compromised VFS could misreport it, but the only
  grant it decides is a session with VFS itself;
- PID 1's service definitions and their capability manifests are compiled into
  `system/init`, which is itself in the archive.

What verification cannot cover is modification after the check by something
that can already write kernel memory: the kernel itself, or a DMA-capable
device. Without an IOMMU, a compromised storage driver can rewrite the archive
in memory (see [threat-model.md](threat-model.md#compromised-driver)). A
verified boot chain protects the system at rest and at boot, not against a
driver that is part of the effective TCB.

## Owner-controlled keys, no lock-in

The keys must belong to the owner of the machine, not to the project or a
firmware vendor:

- the firmware side uses standard UEFI Secure Boot. The owner puts the
  firmware in setup mode and enrolls their own `PK`, `KEK`, and `db`, or
  enrolls the GoOS project certificate if they choose to trust release builds.
  GoOS must not depend on a third-party CA being present in `db`, and must not
  ship a shim that pins a vendor key;
- the system-image key must be replaceable without the project's cooperation.
  With a key built into the kernel, the owner builds the kernel with their own
  key and signs it with their own Secure Boot key, so both links in the chain
  are theirs. Reading trusted keys from the UEFI `db` instead would let the
  owner manage one key store, at the cost of X.509 parsing in the kernel;
- a machine with Secure Boot disabled must still boot GoOS. Verification is a
  property the owner turns on, and the boot banner should say whether it is in
  effect, rather than the system refusing to run.

## What exists and what is missing

Already in place:

- a single, immutable system image that holds every program, the command
  index, and the static configuration of PID 1 (ADR 0070, 0080);
- launch resolution in the kernel, confined to that image (ADR 0022, 0030);
- read-only mappings of the image for the one service that serves it, and
  never-writable shared image pages (ADR 0070, 0081);
- a single point in the kernel, between `LoadInitramfs` and the first lookup,
  where the archive is complete and not yet parsed;
- mutable state kept apart from the image on `/state` and user data on
  `/home`, so a signed image does not have to cover data that changes
  (ADR 0071);
- deterministic artifacts: the archive is written with fixed metadata and
  epoch timestamps, and the kernel is linked without a PE timestamp, so an
  owner can rebuild a release from source, compare it, and sign it with their
  own keys.

Missing:

- a Secure Boot-capable OVMF build and a variable store in the QEMU
  configuration, and instructions for enrolling keys;
- key generation and a signing step in the build for `goos.efi`;
- a signature format for the system image, a signing step in
  `tools/initramfs`, and a verifier in the kernel. It would run once at boot,
  where ADR 0065 permits allocation;
- rollback protection (a version the kernel refuses to go below) and
  revocation beyond what `dbx` gives for the kernel;
- a recovery path when verification fails, which today would simply be a
  halted boot;
- measured boot (TPM PCR extension) for remote attestation, which spec §40
  does not require.
