# ADR 0084: Filesystems out of the normal kernel

## Status

Accepted. Extends ADR 0058.

## Context

The kernel is meant to be as small as possible, and filesystems belong in
userspace services. The RAMFS and VFS libraries under `kernel/ramfs` and
`kernel/vfs` are run by the filesystem service; the kernel itself serves no
file operation.

The kernel still imported both packages. `kernel/main.go` declared a boot
RAMFS, a device RAMFS, a VFS namespace, and two readback buffers, and
`kernel/selftest.go` exercised them in place as part of the validation
self-tests (ADR 0058). Those self-tests are reached only when the constant
`validation.Enabled` is true, so in a normal build the calls were dead code.
Whether the dead code and its state left the binary depended on the
compiler's elimination, not on the source: the normal kernel's source named
two filesystems, and its image carried about 5 KiB of reflection and
string-conversion support that only the self-test path needed.

## Decision

**The RAMFS and VFS self-tests are compiled only into validation builds.**
They move from `kernel/selftest.go` to `kernel/filesystem_selftest_on.go`,
built with `goos_validation`, together with the filesystems and buffers they
use. `kernel/filesystem_selftest_off.go`, built without the tag, defines the
same entry point as an empty function. Neither `kernel/main.go` nor
`kernel/selftest.go` imports `goos/kernel/ramfs` or `goos/kernel/vfs`.

This follows the pattern `internal/validation` and `kernel/diag` already use:
a build tag selects a file, and code outside it calls one entry point that
exists in both variants.

**The boot banner drops its toolchain line.** A normal boot prints the release,
a blank line, and then CPU, memory, timer, security, and scheduler. Which
compiler built the kernel is a property of the build, not of the running
system, and `uname` does not report it either.

## Alternatives

**Rely on dead-code elimination.** The call is guarded by a constant, and the
filesystem code itself was already absent from the normal image. But the
guarantee lived in the optimiser, the image still carried support code the
guard did not remove, and the normal kernel's source still declared
filesystems it never used.

**Tag the whole of `kernel/selftest.go`.** Every self-test would then leave the
normal build at once. The file also holds the TTY checks and the post-boot
checks called from other files, which are changing independently; moving the
filesystem part alone is the smallest change that removes what does not belong
in the kernel.

## Consequences

The normal kernel image links no symbol of `goos/kernel/ramfs` or
`goos/kernel/vfs`; its `goos.sym` names only the initramfs reader, which the
kernel needs to find `system/init`. At 100 Hz the image shrinks from 280064 to
274432 bytes: 5120 bytes from the filesystem self-tests and 512 from the
banner. The validation build is unchanged and still prints the `RAMFS_*_OK`
and `VFS_*_OK` markers.

Other validation-only code is still linked into the normal kernel: the
scheduler demonstration tasks `goos_demo_task_a` and `goos_demo_task_b`, which
the scheduler bootstrap creates in every build and which sleep at once in a
normal one, and the assembly probe `goos_probe_address_spaces`. Removing them
touches the scheduler bootstrap and the assembly object, and is left for later.
