# Native images

GoOS native executables use a small versioned envelope independent of Go
structure layout. The current format is a bootstrap format, not a declared
stable application ABI.

The 32-byte header contains the eight-byte `GOOSNAT\0` magic, format version,
header size, segment count, reserved field, entry address, and initial stack
pointer. It is followed by fixed 32-byte segment records containing virtual
address, payload offset and size, in-memory size, permission flags, and a
reserved field. All integers are little-endian.

The loader accepts at most eight segments and 128 pages across the complete
image. Every segment must be page-aligned, lie in the current userspace range, have a bounded
payload, and request known permissions. Writable-executable mappings are
rejected. Entry must lie inside an executable segment; the aligned initial stack
pointer must lie inside a writable non-executable segment. Overlapping virtual
ranges, truncated headers, invalid versions, reserved fields, offset overflow,
occupied destination mappings, and unsupported page-table branches are rejected
before pages are exposed.

`elf2native` stores only a segment's bytes up to its last non-zero byte; the
stack and heap reservations of a TinyGo image are therefore not part of the file.
For each accepted segment the loader allocates and zeroes every required
physical page, copies bounded payload chunks through the kernel identity map,
and only then installs the final user mappings. This preserves W^X while
loading executable bytes. The loader returns only the validated entry and stack
pointer needed to create a userspace Task.

Relocations, executable ASLR, demand paging, and mapping rollback after an
internal VM failure remain future work.
Linux ELF is a separate compatibility-layer format and does not define this
native envelope.

Process startup is a separate versioned ABI described by ADR 0031. The current
`GST2` block is limited to 128 bytes and carries command, working directory,
bounded `argv` and environment vectors, and named process-local handle bindings. Keeping startup
metadata outside the image format allows one immutable image to run under
different capability manifests.
