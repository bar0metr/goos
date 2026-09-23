# ADR 0077: Refusing a volume without losing the system

## Status

Accepted. Extends ADR 0036 and ADR 0060.

## Context

Everything the FAT32 backend computes — where the FAT begins, where a cluster
is, how far a chain may run, how many clusters exist — is derived from fields in
the image's boot sector. The image is on a device the system does not control
and is untrusted input in the same sense an ELF file or an IPC message is.

The mount checked some of those fields: the signature, the sector size, one
sector per cluster, one FAT, a plausible reserved count and root cluster. It did
not check the two things that bound everything else. The declared volume size
was never compared against the device, so an image could claim four billion
sectors on a sixty-four megabyte disk; the derived cluster count then bounded
every chain walk, and `freeChain` would iterate four billion times, each step a
device read and a write. The FAT size was never checked against the cluster
count it has to describe, so a walk could read entries out of the data region
and follow whatever a file's contents happened to look like.

The second problem was what happened when the mount did refuse. The filesystem
service treated a failed mount as fatal. A corrupt volume therefore killed VFS,
which is the service PID 1 starts the shell behind, so the system did not boot.
The one state from which a person could look at the volume was the one state the
volume denied them.

## Decision

**The geometry is checked against the device before an offset is derived from
it.** The device reports its capacity, the declared volume must fit it, the
metadata must fit the volume, and the FAT must be large enough for an entry per
cluster plus the two reserved ones. An image that does not describe itself
consistently is refused rather than approximated.

**Capacity is part of the device interface.** The block service already knows
it; the filesystem could not ask. It is asked once per mount and cached, because
it does not change while the device is attached.

**A refused volume is not fatal.** The filesystem service reports it, prints one
line on the console, and serves the rest of the namespace. The volatile root and
the read-only system image are already mounted by then, so the machine comes up
to a prompt with no persistent storage rather than to nothing at all.

**The namespace says what is there.** `/persist`, `/home` and `/state` are
simply absent, and `ls /` shows what is actually mounted. Nothing pretends the
volume is there and fails later.

**Services that need a file tolerate not having one.** The log service warns and
keeps draining the kernel ring, which is the system log; it retries opening its
file on each pass, so a volume that is repaired starts being written to without
a restart. The configuration service serves its declared defaults. Neither is a
reason for the system to stop, and both are needed to diagnose why the volume
was refused.

**The geometry this implementation handles stays deliberately narrow.**
512-byte sectors, one sector per cluster, one FAT. These are refused rather than
supported because a valid FAT32 volume with any other geometry is one this code
would misread, and misreading a filesystem is worse than declining it.

## Alternatives

**Trusting the image and letting reads fail.** Offsets past the end of a device
already fail, so a lying image degrades to I/O errors without a geometry check.
It also leaves loop bounds derived from the lie: a four-billion-step chain walk
that fails at every step is not an I/O error, it is a hang.

**Repairing the volume.** A mount could correct a FAT that disagrees with the
boot sector, or rebuild from the second FAT. There is no second FAT here by
construction, and a filesystem that guesses at what a damaged volume meant is
how one loses data rather than how one keeps it. Refusing and reporting leaves
the decision with the person.

**Mounting read-only when the geometry is suspect.** Halfway between accepting
and refusing, and it sounds cautious. The checks that fail are the ones that
bound reads, not writes, so a read-only mount of an image that fails them is
exactly as unsafe as a writable one.

**Keeping the mount fatal and relying on a rescue image.** Honest, and it is
what initramfs is for. It also means a single bad sector on the persistent
volume turns a running system into one that needs another boot medium, when
everything except the volume still works.

## Consequences

`make test-boot` gains a phase that boots the system on a volume claiming far
more sectors than its device holds. The system reaches a prompt, reports
`[WARN] no persistent storage`, lists a root with no persistent mounts, runs a
command, and answers a configuration query. A corrupt disk costs the storage,
not the system.

Twelve malformed boot sectors are refused by unit tests, including a volume
larger than its device, a FAT too small for its clusters, a volume of metadata
only, a root cluster outside the volume, and a device whose size is not a whole
number of sectors. A refused mount leaves no usable filesystem behind, so a
later lookup cannot act on geometry the mount rejected.

The filesystem no longer emits `FAT32_MOUNT_OK` when it did not mount anything,
so the marker means what it says.

Crash consistency is still not addressed. A write interrupted between the FAT
and the directory entry leaves a volume this implementation will mount and read
inconsistently, because the geometry is intact and only the contents disagree.
That needs journalling or a second FAT, and it is a different decision.
