# ADR 0175: The installed system runs from the volume, with an archive recovery mode

## Status

Accepted. Amends ADR 0080, ADR 0093, ADR 0097, and ADR 0121: the boot
archive is no longer mounted at `/system`, and the command index and the
programs it describes are installed on the volume.

## Context

Until 0.2.1 the boot archive held the whole distribution and the VFS mounted
it at `/system`. PID 1 launched every service and every command of
`/system/bin` from it by extent, and the persistent FAT32 volume held only
user data and service state. Every change to a command meant a new archive,
the archive's size limit bounded the distribution, and a system could not
exist apart from the media it booted from.

Moving the system onto the volume raises four questions. What runs before the
volume is up, and what runs when it never comes up. Which bytes a program
name means, now that a name can lead to the archive or to a file. How the
installed system stays unmodified when the same FAT32 volume also holds
writable user data under several names. What happens when the services that
serve the volume fail under a running system.

## Decision

### Namespace

The volatile RAMFS stays the root. The boot archive is mounted read-only at
`/bootstrap`, under which its member `system/X` appears as `/bootstrap/X`;
the kernel still loads PID 1 as the member `system/init`. The volume's
`system` directory is published at `/system` and its `apps` directory at
`/apps`, beside the existing `/persist`, `/home`, and `/state`. `/run` and
`/tmp` are RAMFS directories. The VFS keeps `/bootstrap`, `/system`, and
`/apps` reserved: nothing can be created at or below them, or moved there,
while nothing is mounted there.

The installed system is:

| Path | Contents |
|---|---|
| `/system/bin` | the native commands |
| `/system/services` | the shell, configuration, and log services |
| `/system/linux` | static Linux programs, the validation fixtures among them |
| `/system/commands` | the command index, in the existing format |
| `/system/motd` | the message of the day |
| `/system/release` | the installation record |

Configuration stays in `/state`. The build installs a system on a disk image;
a guest never creates one.

### Protection by identity

The VFS protects the volume's system directory from the moment it mounts the
volume, before PID 1 reads anything in it. The namespace records the
directory as a node of one backend. Every path walk notes whether it reached
that node, so the check covers every name that leads there: `/system`,
`/persist/system`, any spelling FAT32 treats as the same name, and a mount of
the directory itself. Create, a writable open, truncate, unlink, rename, and
replace are refused at or below the node, and a rename or replace that would
move a name into it or out of it is refused too. The protection survives a
restart of the filesystem service, whose attach is checked to serve the same
volume with the system directory where it was. A stat reports protected
files read-only. FAT32's own read-only attribute plays no part in the
decision.

### Boot

PID 1 starts the VFS, block, driver, and FAT32 services from the archive by
extent, as before, and waits for the VFS to answer the attach, at most 20
seconds. It then decides the mode:

1. A `system/force-recovery` member in the archive asks for recovery.
2. No attached volume means recovery.
3. Otherwise PID 1 reads, through `/persist/system`, the installation
   record, the headers of the three session service images, the command
   index, and the header of every program the index describes.
4. If all are acceptable it asks the VFS to publish `/system` and `/apps`.
   Both appear or neither does; a refusal withdraws them and means recovery.

It prints `[MODE] normal`, or `[MODE] recovery:` with the stage, path, and
reason. In normal mode it starts the configuration, log, and shell services
from `/system/services`; the terminal services stay in the archive. In
recovery mode it starts only the terminals and the archive's shell. Neither
configuration nor logging runs there: the shell does without them, and each
would hold files on the volume a check needs to itself.

### Installation record

`/system/release` is at most 512 bytes of `name value` lines: `format`,
`arch`, `release`, `startup`, `image`, and `index`. The format comes first
and a format other than 1 is refused before anything else is read. Each field
appears once, no other field may appear, and values are short and drawn from
a fixed character set. The architecture, the release, and the versions of
the startup block, the native image format, and the command index format must
equal PID 1's own. A different release is refused and reported; nothing on
the volume is changed. This checks compatibility. It verifies nothing about
who wrote the volume.

### Executable origin

A path under `/bootstrap` names an archive member, launched by extent. Every
other path names a file, read through the VFS into a file-backed executable
source (ADR 0121). A file missing from the volume is never looked for in the
archive. Services are launched the same way. Once the system is up, a
service's program, and the program an application asks PID 1 to run for it,
is read as a job's programs are: PID 1 starts the read, the supervisor holds
the launch as deferred, which spends none of the restart budget, and the
main loop waits on the read with everything else. The launch runs when the
read is answered, or fails when its deadline passes, so a VFS that does not
answer costs the launch and never stops PID 1 from serving. The boot, which
starts services in order before the main loop, reads them in place.

The kernel copies a native call's buffers without taking file-backed page
faults. The userspace runtime therefore touches every page of a buffer
before it passes it to the kernel, which brings in a constant or a variable
of a program read from a file.

### Grants

A command is granted what the index that describes it says, and only that
index: the archive's recovery index for `/bootstrap/bin/X` launched from the
archive, and the installed index for `/system/bin/X` read from the protected
volume, only in normal mode while the system is published. Any other program,
whatever its name, gets its standard streams and nothing more. A copy of
`fsck-fat` in `/home/user/bin` cannot check a volume.

### Search

PID 1 starts the shell with `PATH=/system/bin:/home/user/bin` in normal mode
and `PATH=/bootstrap/bin` in recovery. The shell looks a bare name up in each
absolute directory of its PATH in turn and never in the working directory. A
Linux program given either native default gets `PATH=/system/linux`, as in
0.2.1.

### Restarts and recovery

The storage services always restart from the archive. When the VFS, block,
driver, or FAT32 service goes, PID 1 holds the session services back: none
is started from the volume until the storage behind it is back. A restarted
FAT32 service is attached again, the system republished, and its record and
index read again. A restarted driver alone makes the volume usable again at
once. A program read from the volume can end when its source fails;
recovering the storage gives each session service a fresh restart budget.

When a storage service or a session service uses up its restarts in normal
mode, PID 1 enters recovery for good: it withdraws the installed system, or
does once the programs using it have exited, stops the configuration and log
services, and starts each shell again from the archive. A running shell is
stopped first, so a console never has two. Returning to normal mode takes a
reboot.

### Checking the volume

An exclusive `fsck-fat` check needs the volume unmounted. In normal mode the
running system's programs come from it, so PID 1 refuses the check with
`LaunchVolumeBusy` and unmounts nothing. In recovery mode the check runs as
before.

### Build

`tools/initramfs` builds the production archive from PID 1, the storage
services, the terminal, the shell, the message of the day, and the recovery
commands `cat cp df echo edit fsck-fat ls mkdir mount mv pwd rm stat touch
write`, with an index cut down to them. A validation archive adds
`fault-probe`, the generated raw Linux probe, and `ps`, kept in a list of
its own. `tools/sysimage` lays out the installed system from the build's
outputs, and `mkfat32 -tree` copies it
onto a volume in name order. `make run` makes `build/goos-disk.img` only when
it does not exist. `make disk` refuses an existing disk; `make disk-overwrite`
replaces it. `make run RECOVERY=1` boots an ISO whose archive asks for
recovery.

## Consequences

The production archive is 3.4 MiB of the unchanged 6 MiB limit, and the rest
of the distribution is no longer bounded by `boot.MaxInitramfs`. A system on a disk runs only with a
boot archive of the same release; a newer archive boots an older disk into
recovery until the disk is installed again.

A storage fault under a normal system ends programs read from the volume,
the shell among them, and the shell comes back once the storage has. This is
the file-backed source behavior of ADR 0121, not something this change hides.

The checks prove compatibility, not integrity. Anyone who can write the disk
image outside GoOS can change the installed system; inside GoOS no client
can. This is neither a verified boot chain nor an update system: there is no
installer, upgrade, rollback, or signature.

A machine without a virtio-blk device boots: the kernel gives the driver
slots an object without port or DMA authority, the driver fails, and PID 1
enters recovery.
