# ADR 0066: FAT32 long names

## Status

Accepted. Extends ADR 0060.

## Context

The FAT32 backend at `/persist` stored one directory entry per file, and that
entry carried an 8.3 name. A request whose name did not fit eight characters, a
three-character extension, and the small alphabet the encoder accepted was
rejected before it reached the disk, so `write /persist/startup-block-
regression.txt "x"` failed with "create failed" and the boot test had to name
its files in the same cramped form the rest of the system had outgrown. The
8.3 encoder also folded case: a name written as `Notes.TXT` was stored upper
case and read back lower case, so it did not round-trip.

Everything above the backend was already wide enough. A VFS path component may
be `ramfs.MaxName` bytes, the GVF1 directory reply carries a name of the same
width, and the shell's line editor and argument vectors comfortably carry a
path built from such components. Only the backend was missing, exactly as
subdirectories were missing before ADR 0060.

VFAT solves this on disk with a chain of entries that precede the real one.
Each carries the long-name attribute `0x0f`, an ordinal byte counting down to
one with `0x40` marking the first entry written, thirteen UCS-2 characters
split across three runs of the entry, and a checksum of the 8.3 name that
follows. An implementation that does not understand the chain sees entries with
the volume-label bit set and skips them, which is why the format has survived.

## Decision

**Names are bounded at `ramfs.MaxName`, sixty-three bytes.** VFAT allows 255,
but a name wider than sixty-three cannot cross a VFS path component or fit a
directory reply, so the larger bound would be a promise the system cannot keep.
A longer name is rejected with invalid argument rather than truncated, and so
is a name holding a byte outside printable ASCII, one of the characters FAT
reserves (`" * / : < > ? \ |`), or a leading space or trailing space or dot.

**A name that fits 8.3 keeps the old single-entry layout.** "Fits" now means
lower case as well as short, because the 8.3 form is read back lower case and a
name holding upper case would not survive the round trip. `hello.txt` is still
one entry whose name field reads `HELLO   TXT`; `Hello.TXT` gets a chain.

**A name that does not fit gets an alias and a chain.** The alias takes the
first six name characters, uppercased, with anything outside the 8.3 alphabet
replaced by `_`, then `~N`, then up to three characters of the last extension.
`N` counts from one, dropping the base to five characters once it reaches two
digits, and the directory is scanned for each candidate until no live entry
holds that 8.3 name. A directory holds at most sixty-two entries, so the ninety-
nine candidates are always enough.

**Reading tolerates damage.** A chain is accepted only when its ordinals count
down without a gap, its padding sits at the tail, every code point is printable
ASCII, and the checksum matches the 8.3 name of the entry that follows. A chain
that fails any of these is discarded and the short name is used, which is what
other implementations do with a directory that a careless writer has edited.

**Matching is on the resolved name.** Lookup, create, rename, and unlink
compare the requested name against the name each entry is listed under — its
long name when the chain is intact, its 8.3 name otherwise — case-insensitively.
One comparison therefore covers both layouts, and the case-insensitivity that
8.3 always had is preserved rather than lost at the boundary between them.

**Allocation takes a run, not a slot.** A chain of up to five long entries plus
its short entry needs six consecutive free slots. Entry indices are linear over
a directory's whole cluster chain, so a run may span a cluster boundary and no
special case is needed for a chain that does not fit what is left of the
current cluster. When no run is free the directory grows by one zeroed cluster,
under the four-cluster bound ADR 0060 set, which is unchanged.

**Delete and rename move the whole chain.** Unlink marks every entry from the
chain start to the short entry deleted, short entry first, so a scan never
finds a live short entry whose chain has already gone. Rename writes the new
chain ending at the same index whenever the new chain is no longer than the old
one, which keeps the node ID stable, and otherwise takes a fresh run and erases
the old one. Writes go out long entries first and the short entry last, so a
scan that races a create stops at the end marker instead of reading a short
entry without its name.

The per-operation name buffers — the decoded request, the chain scanner, and
the resolved name — live in the `FileSystem` structure. One request is in
flight at a time, so nothing on this path allocates, in the spirit of ADR 0065
even though the VFS service has a heap of its own.

## Alternatives

**Widening the short name instead.** A private eleven-plus-N byte name field
would be simpler to write and would need no chain, but the result would not be
a FAT32 filesystem: the image is read by the host and by QEMU's own tooling,
and an incompatible directory would end that.

**Storing the long name in a side file.** A table mapping aliases to long names
would avoid touching the entry format, but it needs its own allocation, its own
crash story, and its own bound, and it would be invisible to every other
reader of the disk.

**Taking the full 255-byte VFAT bound.** It would need twenty long entries per
name, a directory bound far above four clusters, and a wider name in the GVF1
reply and in every path buffer. None of that buys anything while a path
component is sixty-three bytes.

**Keeping upper case inside the 8.3 form.** Treating `Hello.TXT` as a short
name would avoid a chain, but the name would still be read back as
`hello.txt`. Spending a chain on it is what makes the round trip honest.

## Consequences

`write /persist/startup-block-regression.txt "x"` works, and so does a long
name inside a long-named subdirectory. The boot test creates one, reads it
back, and finds both names in `ls` output.

A long-named entry costs up to six of a directory's sixty-two slots, so a
directory of long names holds about ten of them. Create returns resource
exhausted beyond that, as it did before at its own bound.

A rename that needs a longer chain than the old entry occupied moves the entry,
and its node ID changes. VFS invalidates its path cache after every rename, so
a path resolves correctly; a handle opened before such a rename does not, which
matches how an entry deleted under an open handle already behaved.

`ls` rescans a directory from the start for each entry it returns, because a
chain precedes its short entry and a cursor in the middle cannot see it. A
directory is at most sixty-four entries over four cached sectors, so the cost
is bounded and no device traffic is added.

An orphan chain — one whose short entry never arrived, or whose checksum a
careless writer broke — is ignored on read, and the entry it precedes is found
under its 8.3 name instead. Its slots are not reclaimed, because nothing on
disk distinguishes an orphan from a chain this implementation simply does not
understand. A directory scrubber would be the place to collect them, and there
is none.
