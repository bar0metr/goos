# ADR 0071: Persistent volume layout

## Status

Accepted. Extends ADR 0036 and ADR 0069.

## Context

The FAT32 volume was mounted whole at `/persist`, and that was the only
persistent name in the system. There was no `/home`.

This diverged from the specification twice. The acceptance criterion for
persistent storage is written against `/home/demo`, and the roadmap had quietly
rewritten it to `/persist` with no decision recorded. More substantially, the
architecture calls for the system image, mutable system state, and user data to
be distinguishable, because a future image update replaces one and must not
touch the others. One flat mount distinguishes nothing: a user file and a piece
of service state sit side by side with nothing marking which is which.

The layers below were already able to express the distinction. FAT32 has
subdirectories since ADR 0060, the namespace can mount a backend at any node,
and after ADR 0069 a mount is visible in its parent's listing.

## Decision

**User data and mutable system state get their own mounts on the same volume.**
`/home` and `/state` are mounted on the volume's `home` and `state`
directories, which the filesystem service creates on a volume that does not
have them. A name already taken by a regular file is an error rather than
something to overwrite.

**One device, three names.** A second partition or a second device would
express the same separation physically, but partitioning is machinery the
system does not have, and the point of this decision is that the layout is
named, not that it is physically enforced. When a real update scheme arrives it
will need the names first.

**`/persist` stays as the whole volume.** It is what the boot tests, ADR 0036
and the existing tooling name, it is the only way to inspect the volume as a
whole, and removing it would be churn for its own sake. `/persist/home` and
`/home` are therefore the same directory under two names, backed by one
`fat32.FileSystem` with one sector cache, so the two are coherent rather than
two views that can diverge.

**The aliasing is made safe rather than hidden.** ADR 0069 refuses to remove a
directory a mount points at, by node identity and not only by path, which is
exactly the case `rm /persist/home` would otherwise have been.

**The root filesystem stays volatile.** `/` and `/tmp` remain RAMFS. Nothing
was moved onto persistent storage by this change except by being given a name.

## Alternatives

**Replacing `/persist` with `/home` and `/state`.** Cleaner in the sense that
each path means one thing, and it removes the aliasing entirely. It also breaks
every existing test, the tooling that inspects the image, and ADR 0036, and it
leaves no way to see the volume as a whole. Worth revisiting when a typed
configuration service gives `/state` a real shape.

**Mounting only `/home`.** The acceptance criterion needs nothing else. But
service state is the other half of what the image-update split is for, and
adding it later would mean moving files that services had already written to
`/persist`.

**Separate FAT32 volumes on separate virtio devices.** Real isolation between
user data and system state, and a step towards the A/B image scheme. It needs a
second device in the QEMU invocation, multi-device support in the block service,
and a mount policy, none of which the current milestone needs.

## Consequences

The specification's acceptance criterion runs verbatim: `mkdir /home/demo`,
`write /home/demo/message.txt "persistent"`, `cat`, `reboot`, `cat`. The boot
test performs it through `/home`, and also reads the same file back through
`/persist/home` to assert the two names are one directory.

`ls /` shows `home/` and `state/` beside `persist/`, so the layout announces
itself.

The namespace now holds five mounts of the eight `MaxMounts` allows: `/`,
`/system`, `/persist`, `/home` and `/state`. A sixth is available for the
configuration or log service; beyond that the bound has to rise.

Nothing writes to `/state` yet. It exists so that the first service that needs
durable state has a place that is not the user's home directory.
