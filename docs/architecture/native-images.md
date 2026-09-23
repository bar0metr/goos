# Native images

GoOS native executables use a small versioned envelope independent of Go
structure layout. The current format is a bootstrap format, not a declared
stable application ABI.

The 32-byte header contains the eight-byte `GOOSNAT\0` magic, format version,
header size, segment count, reserved field, entry address, and initial stack
pointer. It is followed by fixed 32-byte segment records containing virtual
address, payload offset and size, in-memory size, permission flags, and a
reserved field. All integers are little-endian.

The loader accepts at most eight segments and 256 pages across the complete
image. Every segment must be page-aligned, lie in the current userspace range, have a bounded
payload, and request known permissions. Writable-executable mappings are
rejected. Entry must lie inside an executable segment; the aligned initial stack
pointer must lie inside a writable non-executable segment, and the page
directly below that stack segment must be a guard page: inside the user range
and covered by no segment (ADR 0083). The loader also refuses a guard page the
kernel maps itself after loading. Overlapping virtual
ranges, truncated headers, invalid versions, reserved fields, offset overflow,
occupied destination mappings, and unsupported page-table branches are rejected
before pages are exposed.

`elf2native` stores only a segment's bytes up to its last non-zero byte; the
stack and heap reservations of a TinyGo image are therefore not part of the file.
The userspace linker script emits the stack as a fourth `PT_LOAD` segment,
separated from the data and heap segment by one unmapped page, and
`elf2native` runs the loader's parser on its output, so a layout without the
guard fails the build.
For each accepted segment the loader allocates and zeroes every required
physical page, copies bounded payload chunks through the kernel identity map,
and only then installs the final user mappings. This preserves W^X while
loading executable bytes. The loader returns only the validated entry and stack
pointer needed to create a userspace Task.

Read-only segments of images from the system image are loaded once and their
frames shared by every process running the image (ADR 0081).

Relocations, executable ASLR, demand paging, and mapping rollback after an
internal VM failure remain future work. Images are static `ET_EXEC` programs
linked at `0x40002000`; what randomizing their placement would change in this
format is described in [memory.md](memory.md#aslr-readiness).
Linux ELF is a separate compatibility-layer format and does not define this
native envelope.

Process startup is a separate versioned ABI described by ADR 0031. The current
`GST3` block (ADR 0062) is limited to 1024 bytes and carries command, working directory,
bounded `argv` and environment vectors, and named process-local handle bindings. Keeping startup
metadata outside the image format allows one immutable image to run under
different capability manifests.
