# ADR 0097: Executable objects

## Status

Accepted. Extends ADR 0093.

## Context

ADR 0093 took path resolution out of the kernel: a launch names an extent of
the system image and a handle with Execute on it. That left the system image as
the only executable source. A program on the persistent volume could be
copied there, listed and read, but not run, and the next milestones need
exactly that: Linux ELF programs and, later, programs built inside GoOS live on
the volume, not in the boot image.

The kernel must not learn a second path syntax or a filesystem to close the
gap (ADR 0093), and the VFS must not be on the critical path of a launch it
cannot answer for. The existing shared-memory objects are the wrong vessel:
they are created only by the kernel at boot, hold at most 16 pages, can be
mapped writable by any holder, and by design never carry Execute (ADR 0013).
A program the kernel validates and then loads must not change in between.

## Decision

**An executable object is kernel memory filled with one program.** PID 1
creates it with `ExecutableCreate` (native operation 37), giving its exact size
in bytes, at most 1 MiB (`loader.MaxImagePages` pages, the most a native image
can map). The pages are allocated zeroed and charged to PID 1's page quota. It
fills the object with `ExecutableWrite` (operation 38), which appends at most
4 KiB per call and copies from PID 1's memory; a write that would pass the
object's end is refused whole.

**An object seals itself when its last byte is written.** From then on it
accepts no write, and only then can it be launched. It is never mapped into any
process: its bytes enter only through `ExecutableWrite` and leave only through
the loader. So a sealed object is immutable without tracking mappings, and the
bytes the loader validates are the bytes it loads.

**The loader capability authorizes creation.** A new fixed device object,
`LoaderObject`, is installed in PID 1 at boot as handle `0x100000017` with
Write and without Transfer. No other process can create an object. The object
handle PID 1 receives carries Write, to fill it, and Execute, to launch it,
and no Transfer: the object has one holder and is freed, its pages returned
and uncharged, when that handle closes or its holder exits.

**A launch names an object as it names the image.** The lifecycle request is
unchanged: `{handle, offset, length}`. When the handle is the system image with
Execute, the extent is an extent of the image, as before. When it is an
executable object with Execute, the object must be sealed and the request must
name all of it: offset zero and the object's length. The loader reads both
through one `loader.Source`, contiguous for the image and paged for an object,
and validates both the same way. An object's pages are loaded privately; the
shared read-only page cache stays keyed by image extent, since an object's
identity says nothing about its bytes.

**PID 1 decides what may run and reads it.** A command path under `/system` is
looked up in the image as before, and `/system` outside `/system/bin` still
holds services, which are not commands. Any other canonical path is read
through a VFS session of PID 1's own: it stats the file, refuses anything but
a regular file of 1..1 MiB, creates an object of that size, reads the file into
it in chunks, and closes the file. The copy must end exactly at the size the
file had when it was stated, so a file that grew or shrank while it was read is
refused rather than launched as a mixture. PID 1 closes the object once the
kernel has answered the launch.

**A program from the filesystem gets its streams and nothing more.** Command
grants (ADR 0080) are the commands of the immutable image, matched by their
name there. A program read from the filesystem receives standard input, output
and error, whatever its name; in particular no VFS session.

**The shell runs program paths.** A command word containing a slash is a path,
absolute or relative to the working directory; a bare word is still a name in
`/system/bin`. The exec protocol accepts any canonical absolute path as a stage
(version 10); a failure names the program by its path.

## Alternatives

**Mapping a shared-memory object writable into PID 1, then sealing it.** The
object would be filled at memory speed rather than through a copy per 4 KiB,
but sealing would have to find and revoke every writable mapping, which the
kernel does not track. The copy through `ExecutableWrite` costs one kernel entry
per 4 KiB against the many IPC exchanges the file read already costs.

**Having the VFS or the filesystem service produce the object.** It is the
shape a page cache will want, but it would give the VFS the loader capability
or the kernel a filesystem-shaped object, and the page cache does not exist
yet. PID 1 filling the object keeps the authority where launches already are.

**Passing the kernel a file handle.** The kernel would have to read through
the VFS, which is the dependency ADR 0093 removed.

**Sharing an object's pages between launches.** A second launch of the same
file would map its read-only segments from the first. It needs an identity for
the bytes, such as a content hash, and the file is read again for every launch
anyway, so it is left until a page cache makes the read cheap.

## Consequences

Programs run from anywhere in the namespace outside `/system`:
`/home/demo/args`, `./args`, and a path as a pipeline stage. The kernel still
resolves no path and parses no filesystem; the only name it knows is the
bootstrap `system/init`.

The kernel gains two native operations and one kernel table of two objects,
each at most 256 pages. The largest program a file can hold is 1 MiB. A
launch from a file costs a VFS stat, open and close and one read exchange per
224 bytes of the file. Under QEMU TCG at 100 Hz, `time /home/args` (22 KiB on
the FAT32 volume) took 113 ms against 14 ms for `/system/bin/args`, and a
60 KiB program 291 ms. Launches from the system image are unchanged. Every
exchange allocates in PID 1's arena, so PID 1 rewinds the arena before each
read, once the previous chunk is in the object.

`cp` did not rewind its arena per chunk as `cat` does; without it a copy
stopped after a few hundred bytes with the target truncated, which made it
impossible to put a program on the volume. It now rewinds per chunk.

The validation build proves the rules at three levels. The kernel creates an
object from PID 1's capability before PID 1 runs and requires it to refuse a
foreign capability, a zero or oversized size, a bad result pointer, a write
past its end or after sealing, and mapping, and to return every page and the
quota when closed (`EXECUTABLE_OBJECT_OK`). The shell requires every handle
it holds, and PID 1's loader slot, to be refused (`EXECUTABLE_CREATE_DENIED_OK`).
PID 1 requires an unsealed object, a part of a sealed one, a closed one, and
one holding a text file to be refused with no process created. The boot test
copies `args` to `/home`, runs it by absolute and relative path and in a
pipeline, runs it again after a reboot, and requires a data file, a missing
path and a service of the image to be refused.

PID 1 reads with its own VFS session. Every session sees the same namespace
today, so this reads nothing the shell that asked could not read. When
sessions get namespaces of their own, the shell will have to hand PID 1 the
open file rather than its path.

A file rewritten at the same size while PID 1 reads it can still launch as a
mixture of the two versions. Whatever launches is validated as a native image
and runs with its streams only, and whoever can write the file could have
written any program into it.
