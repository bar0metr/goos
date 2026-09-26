# ADR 0109: Variable-length startup object

## Status

Accepted. Replaces the `GST3` encoding of ADR 0031 and ADR 0062 and the exec
request encoding of ADR 0064; keeps their named startup handles, staging, and
copy-before-validation rules.

## Context

A process learned its command, working directory, arguments, environment, and
handles from a `GST3` block of at most 1024 bytes. One-byte length and count
fields made the real limits eight arguments, four variables, 255 bytes of
arguments, 255 bytes of environment, a 32-byte command, a 96-byte working
directory, and seven handles with 15-byte names. The shell imposed its own
smaller limits in front of them: eight words of at most 127 bytes each, on a
line of at most 128 bytes, sent to PID 1 in a 1024-byte slot with one-byte
lengths. A command could not be given a dozen file names, and a path longer
than 192 bytes could not be named at all (GoOS 0.1.3 specification §6).

## Decision

**`GST4` is bounded by bytes, not counts.** The startup object is a 32-byte
header — magic, version, handle count, total size, argument count, variable
count, the offset of the string area — followed by a string table of
`{offset, length}` entries for the command, the working directory, every
argument, and every `NAME=VALUE` variable; a handle table of `{name offset,
name length, ID}` entries; and the string area. Counts are 32-bit and limited
only by the object's size. The layout is in `userspace/startup/startup.go`.

**The limits are named and justified.**

| Limit | Value | Why |
| --- | --- | --- |
| whole object | 64 KiB (`kcall.MaxStartupSize`) | a `PATH_MAX` command and directory plus about 56 KiB of arguments and environment; PID 1's staging area stays 256 KiB and the kernel's copy 64 KiB |
| one argument or variable | 32 KiB (`startup.MaxString`) | half the object, so one string cannot crowd out the rest |
| command, working directory | 4096 bytes (`limits.PathMax`) | the native path limit (ADR 0110) |
| handles | 64, names of up to 63 bytes | far above what a manifest grants (seven today) |

Changing the object's size is changing one constant; the staging and exec
areas are sized from it.

**Validation is total and pointer-free.** Decoding checks the magic, version,
and reserved field; that the counts fit the size before anything is multiplied;
that the string area starts exactly after the tables; that every string lies
inside it with 64-bit arithmetic; that no string holds a zero byte, so each can
later be passed to `execve` as a C string; that `argv[0]` and every variable
name are non-empty; and that variable and handle names are unique. The
decoder is fuzzed. Offsets are relative to the object; nothing in it is an
address.

**Built in place, bound in place, read in place.** PID 1 writes a template
into a 64 KiB slot of its staging area with `startup.Writer`, which needs no
allocation. The kernel copies it once, binds the zero handle IDs in its own
copy, and maps the result read-only at `vm.UserStartupAddress` on as many
pages as it needs. A process reads the 32-byte header with `StartupRead`,
which now copies any length up to the maximum, and decodes the object where
it is mapped; `startup.Read` does both.

**The exec request follows.** Version 11 of the shell-to-PID-1 protocol uses
32-bit lengths and counts, carries arguments and variables as length-prefixed
strings, and uses 64 KiB slots of a 256 KiB shared area. The shell writes a
stage into the slot as soon as it has parsed it (`exec.Builder`), so one
tokenizer buffer serves every stage. A request that fits its slot but not a
startup object fails with the launch status "arguments too long".

**The shell's limits are the line's.** The terminal accepts lines of up to
4096 bytes and delivers a line longer than one message in parts: each part but
the last answers a read with `ResultPart`, and the client asks for the next
with `Continue` (terminal protocol version 2). The kernel's console input ring
holds 8 KiB, two whole lines. The tokenizer bounds words by the line's bytes,
not by a count, and the output line of the utilities no longer fails past 256
bytes. `cat`, `rm`, `touch`, and `mkdir` take any number of paths.

## Alternatives

- **Raising the one-byte limits.** Every limit would move again at the next
  use case; the byte bound is what the resources actually constrain.
- **A pointer array like the System V stack layout.** It ties the ABI to an
  address, which ASLR would then have to relocate, and gains nothing a table
  of offsets does not already give. A Linux `execve` layer builds that layout
  from this object in the process's own memory.
- **Delivering long lines through shared memory.** A part request per 255
  bytes costs sixteen exchanges for the longest line, typed by a person; a
  shared page per terminal session would need a new grant and its cleanup.

## Consequences

- A command can be given hundreds of arguments; the boot test runs one with
  700 and one with a 3000-byte argument.
- Every process's startup object costs one page for small ones and up to 16
  for the largest, charged to the process.
- `GST3` programs no longer start: every program in the image is rebuilt with
  the new runtime, and there are no others.
- A future `execve` translates its `argv` and `envp` into this object without
  changing it.
