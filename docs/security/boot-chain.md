# Boot chain and signature verification

**Status: not implemented.** GoOS 0.2.9 validates formats, bounds, release
compatibility and in-guest write policy, but it does not authenticate the
kernel, boot archive or installed system. Whoever can replace the ISO controls
the kernel and bootstrap code; whoever can replace the installed disk can
supply a different but syntactically compatible normal-mode system.

This document describes the current chain and where authenticity verification
would have to be inserted. The boot sequence itself is in
[boot.md](../architecture/boot.md); the resulting trust assumptions are in
[threat-model.md](threat-model.md).

## The chain today

```text
OVMF / UEFI firmware
 └─ ISO El Torito EFI system partition (16 MiB FAT)
     ├─ EFI/BOOT/BOOTX64.EFI        GoOS kernel
     └─ initramfs.tar               boot archive, at most 12 MiB
          ├─ system/init            PID 1
          ├─ bootstrap storage services
          ├─ tty and recovery shell
          └─ recovery commands/index

installed FAT32 disk
 ├─ system/                         protected and published as /system in normal mode
 │   ├─ services/                   normal-mode services
 │   ├─ bin/                        native commands
 │   ├─ linux/                      static Linux programs
 │   ├─ security/                   production trust inputs
 │   ├─ commands                    installed command index
 │   └─ release                     compatibility record
 ├─ apps/                           published as /apps
 ├─ home/
 └─ state/
```

1. **Firmware and kernel.** OVMF finds the no-emulation EFI boot image and
   starts `EFI/BOOT/BOOTX64.EFI`. There is no separate bootloader: the kernel
   is a TinyGo UEFI PE image. The normal QEMU configuration has no Secure Boot
   variable store and no GoOS signing step, so firmware authenticity is not
   established.
2. **Boot archive.** Before `ExitBootServices`, the kernel opens
   `\initramfs.tar` through UEFI file services and reads at most
   `boot.MaxInitramfs` (12 MiB). A larger archive is refused rather than
   truncated. The retained pages become the immutable boot-archive origin used
   for PID 1 and bootstrap/recovery executables.
3. **PID 1.** The kernel loads `system/init` from the boot archive. USTAR
   header checksums detect damage; they are not cryptographic authentication.
4. **Storage bootstrap.** PID 1 launches VFS, block, virtio-blk and FAT32 from
   the boot archive. VFS mounts the archive at `/bootstrap` and the FAT32
   volume at `/persist`, `/home` and `/state`, with the volume's
   `system` directory protected by node identity (ADR 0175).
5. **Normal/recovery decision.** In normal mode PID 1 reads
   `/persist/system/release`, service headers, the installed command index and
   indexed program headers. The release record must match the architecture,
   release, startup format, native-image format and command-index format. This
   is compatibility checking, not provenance checking. If it succeeds, VFS
   publishes the protected volume directories as `/system` and `/apps`.
   Otherwise PID 1 stays in recovery and runs archive-backed tools.
6. **Normal-mode programs.** Configuration, logging, networking,
   `linuxsockd`, remote access, the normal shell and other session services
   are file-backed executables under `/system`. PID 1 reads them through VFS
   into sealed executable sources; the kernel validates the executable format
   and launch capabilities. Native commands use the installed
   `/system/commands` index. None of these checks proves who supplied the
   bytes on disk.

The important 0.2.9 distinction is therefore:

> The boot archive is the bootstrap/recovery origin. The installed FAT32
> `/system` is a second executable origin used by normal mode.

Protecting `/system` from normal namespace writes prevents accidental or
unprivileged in-guest mutation. It is not an authenticity boundary against a
modified disk image or a compromised FAT32 service.

## Verification points

A complete verified chain now needs three independent links:

| Link | Natural verifier | Unit that must be authenticated |
| --- | --- | --- |
| firmware → kernel | UEFI Secure Boot | `BOOTX64.EFI` |
| kernel → boot archive | kernel before first archive lookup | whole `initramfs.tar`, or an archive embedded in the signed kernel |
| boot/archive trust → installed system | a trusted verifier whose result is enforced at every executable/index read | the normal-mode `/system` contents and metadata that grant authority |

### Kernel

UEFI Secure Boot remains the natural first verifier. The owner enrolls the
keys they choose; GoOS must not require a project/vendor key. The build would
sign the final kernel PE before it is placed in the EFI system partition.

### Boot archive

The archive is read through firmware filesystem services, which Secure Boot
does not authenticate. Verification therefore belongs after the kernel has
read the complete archive and before the first `initramfs.Find`.

Two viable shapes remain:

- a detached signature beside `initramfs.tar`, checked by the kernel with a
  public key covered by the signed kernel;
- embedding the archive in the signed kernel PE so the kernel signature covers
  both.

Either design authenticates one immutable archive before any archive-backed
code executes.

### Installed system

Authenticating only the boot archive is no longer enough after ADR 0175:
normal-mode services and commands are read later from FAT32.

A correct design must authenticate the installed system **and bind later
launches to the authenticated bytes**. A one-time hash performed by PID 1
through an untrusted filesystem service is insufficient: the backend could
return different data later.

Acceptable future designs include, for example:

- a signed manifest containing content hashes, with those hashes enforced by a
  trusted executable-source/launch path on every installed executable and on
  the command index; or
- an immutable signed system-image/container format whose authenticated pages
  are the executable origin.

The exact representation is not selected yet. Whatever is chosen must cover
at least the release record, command index, normal-mode service images,
commands, Linux programs that are part of the installed system, and security
policy/trust inputs whose contents affect execution or authority.

Verification failure must prevent normal mode. Recovery should remain
archive-backed so an installed-system failure does not require trusting the
failed volume.

## Owner-controlled keys and no lock-in

The owner of the machine must control the verification keys.

- Firmware verification should use standard UEFI Secure Boot with owner-managed
  `PK`, `KEK`, `db` and `dbx`.
- A kernel-embedded key for the boot archive is acceptable only because the
  owner can build/sign the kernel with their own key. Reusing owner-managed
  UEFI trust is also possible but would require the corresponding parser in
  the kernel.
- Installed-system signing must follow the same rule: no mandatory
  vendor/project key and no network service required to authorize local boot.
- Secure Boot may be disabled by the owner; the system should report the
  resulting verification state rather than silently pretending verified boot
  exists.

The signed trust-store update mechanism used by native TLS is separate from
this boot-chain problem. It authenticates CA bundle updates, not the kernel or
installed operating system.

## What already helps

The present design provides useful structure even without authenticity:

- deterministic kernel, archive and installed-system build artifacts;
- one bounded boot archive with a 12 MiB loader/build limit;
- a stable boot-archive executable origin and sealed file-backed executable
  origins for installed/user programs;
- executable-format, W^X, address and capability validation in the kernel;
- a protected installed `system` directory and a strict compatibility record
  before normal mode is published;
- recovery whose executable set is archive-backed and independent of the
  installed system;
- separation of mutable `/state` and `/home` from installed `/system`.

These properties make verification easier to add, but none is a signature.

## Still missing

- Secure-Boot-capable QEMU/OVMF setup and build signing for the kernel;
- a boot-archive signature format and kernel verifier;
- an installed-system signature/manifest or immutable authenticated image, plus
  enforcement that binds later executable reads to authenticated bytes;
- rollback/revocation policy for installed-system versions;
- a defined recovery/user interface for verification failure;
- TPM measured boot and remote attestation, which are separate from signature
  verification.

Without an IOMMU, a malicious DMA-capable driver also remains inside the
effective TCB: verified boot authenticates what started, not arbitrary memory
writes after boot.
