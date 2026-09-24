# ADR 0070: Read-only system image mount

## Status

Accepted. Extends ADR 0022, ADR 0030 and ADR 0069. The window is four MiB since the ADR 0087 addendum.

## Context

The system had two disjoint namespaces and the user could see only one.

The launch path resolves an executable in the USTAR boot archive. PID 1 turns a
command into `/system/bin/<command>` with the bounded policy of ADR 0030, and
the kernel finds `system/bin/<command>` in the archive with `initramfs.Find`.
The VFS namespace, which is what `ls` and `cat` talk to, was a RAMFS root
holding `/system/motd` and `/tmp` plus the FAT32 mount. Nothing connected them.

So `ls /system` printed `motd` and `ls /system/bin` reported not found, while
`ls` itself was running from `/system/bin/ls`. `/system/bin` was a string in a
policy constant, not a place. The specification's acceptance criterion for the
native application environment is `goos> ls /system/bin`, and it did not hold.
Worse than the missing feature is the divergence: what the user can inspect and
what actually executes were different things, with nothing keeping them equal.

The archive is up to two MiB of firmware-reserved pages the kernel holds after
`ExitBootServices`. A shared-memory object cannot carry it — `shmem.MaxPages` is
sixteen — and copying it into RAMFS would both exceed that filesystem's bounds
and create a second copy that could drift from the one being executed.

## Decision

**The boot archive is mounted, read-only, at `/system`.** It is the same bytes
the launch path resolves, so a listing cannot disagree with what runs.

**A capability authorises the mapping, and only the filesystem service holds
it.** The kernel creates a `device.ImageObject` and installs it in PID 1's
handle table with read and transfer rights. PID 1 hands it to the `vfs` service
in that service's capability manifest and consumes its own copy. No other
process is offered it, and nothing about it is ambient.

**`ImageMap` is a native call that maps and reports.** Given the capability and
the fixed window address it maps the archive into the caller and writes back the
address and length. The mapping is user-readable, never writable and never
executable, so the process that serves the system image cannot alter it, and
W^X holds for the largest mapping in the system.

**The pages are mapped unowned.** They belong to the firmware reservation, not
to the caller, so `DestroyUserAddressSpace` leaves them intact and a restarted
filesystem service maps the same archive again.

**The window has its own leaf page table.** `[0x40200000, 0x40400000)` is
outside the two MiB the bootstrap layout maps, so `ImageMap` calls
`vm.ExtendUserAddressSpace` to add the one page table covering it. Growth is
confined to one level and one page, the page directory pointer and page
directory must already exist, and teardown frees the extra table with the
others. Address spaces that never map an image pay nothing.

**The archive is parsed in userspace.** `initramfs.Archive` is a read-only
`vfs.Backend` over USTAR. Node IDs are header block indices, which are stable
for as long as the archive is mounted; directory listings report direct children
only; hard links, which is how one program answers to twenty names, are followed
by `Stat` and `Read`, so `stat /system/bin/ls` reports the real program size.

**The whole archive is validated at mount time.** `Init` walks every header and
requires the archive to end at a zero block. A truncated or corrupt image is
refused at mount rather than exposing the prefix that happened to arrive, which
is the rule for untrusted input.

**Every mutation is refused with a distinct status.** `ramfs.StatusPermissionDenied`
is new and maps to `vfs.StatusPermissionDenied`, and the utilities report it as
"read-only filesystem". An immutable path is thereby distinguishable from a bad
one, which the old generic "create failed" hid.

**The message of the day moved into the image.** `/system` is a mount now, so
the RAMFS root no longer creates it, and `motd` belongs to the immutable system
image rather than to volatile storage.

## Alternatives

**Serving `/system` from the kernel.** A native call per filesystem operation
would avoid mapping anything, but it puts path resolution and an archive parser
in the kernel, against both the userspace-VFS decision and the rule that
filesystem parsers of untrusted input run unprivileged.

**Copying the archive into RAMFS at boot.** It needs no new kernel mechanism,
but RAMFS bounds are far below two MiB, the copy doubles the memory the image
occupies, and there would be two versions of every program with nothing keeping
them equal.

**Widening `shmem.MaxPages` to 512.** A shared-memory object would then fit the
archive, but `Object` carries its page list inline, so every object in every
process table would grow to four KiB for the benefit of one mapping of
physically contiguous pages that need no page list at all.

**Listing the archive without being able to read it.** Synthesising directory
entries from the archive's headers, with reads refused, would satisfy
`ls /system/bin` with much less machinery. It would also make `/system` the one
directory in the system whose files cannot be read, and `cat /system/motd`
would stop working.

**Resolving launches through VFS instead.** Making PID 1 ask the filesystem
service for an executable would give one lookup path rather than two readers of
one archive. It also puts the filesystem service on the critical path of its own
startup and of every launch, and it is a larger change than the divergence
justifies. The two readers agree because they read the same bytes; unifying them
remains open.

## Consequences

`ls /system/bin` lists the twenty utilities, `stat /system/bin/ls` reports the
size of the program it links to, and `cat /system/motd` reads from the image.
The acceptance criterion for the native application environment holds.

`mkdir /system/backdoor` is refused with "read-only filesystem", and so are
create, unlink, truncate, rename, and write through a handle. The boot test
asserts this alongside the listings.

A lookup or listing scans the archive from the start, which is linear in the
number of members — about thirty — with no device traffic, since the archive is
resident memory. `make test-latency` at 100 Hz puts `ls /system` at 10.4 ms
median against the 9.1 ms of ADR 0057's baseline, and `cat /system/motd` at
9.1 ms against 9.1 ms. The listing now returns eight entries from an archive
scan instead of one from RAMFS; the cost is the scan, and both figures remain
dominated by process launch. Every other case in the table is unchanged.

The filesystem service now fails to start without the image capability, and
reports `VFS_IMAGE_MOUNT_FAILED` if the archive does not validate. A corrupt
boot image stops the system at its mount instead of part way through a listing.

PID 1's capability layout gained one slot, `0x0000000100000015`. The kernel
asserts the whole layout at boot, so a future insertion in the wrong order
panics rather than silently handing a service the wrong authority.
