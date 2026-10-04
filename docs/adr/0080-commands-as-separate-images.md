# ADR 0080: Commands as separate images

## Status

Accepted. Supersedes the multicall image of ADR 0027 and the command table of
ADR 0079 for executables; ADR 0079's table remains for shell builtins.

## Context

Every utility was one multicall image, `/system/bin/coreutils`, with a hard link
per name. Adding a command meant editing four lists that had to agree: the hard
links in `tools/initramfs`, the dispatch switch in the image, the list of names
PID 1 granted a VFS session to, and the help table in the shell. The PID 1 list
is the one the specification forbids — authority decided by a hard-coded list of
command names — and a command missing from it failed at runtime with an
unrelated message.

A multicall image also gives each process the code of every command. Capability
isolation does not depend on that, because authority is granted to the process
and not to the image, but the loader copies the whole image into every process.

## Decision

**Each command is its own program and its own image.** A command is a directory
`userspace/bin/<name>` containing a `main` package. The Makefile discovers the
directories and builds one image per directory into `/system/bin/<name>`.

**A command describes itself in its source.** Each declares
`var manifest = commands.Manifest{...}` with its synopsis, summary, optional
option list, argument bounds and the grants it asks for. The program passes it
to `utility.Run`, which answers `--help` and checks the arguments from it.

**The build turns the manifests into an index.** `tools/commands` reads each
manifest from the syntax tree, validates it — literal fields only, a known
grant, bounds that hold, printable text, a synopsis that starts with the
command's name — and writes `/system/commands`, one tab-separated line per
command. `tools/initramfs` refuses an image without an index entry and an entry
without an image.

**PID 1 grants from the index.** It reads the index once through a VFS session
of its own and grants a VFS session to a command whose entry asks for one. A
command without an entry gets the standard streams only. The index lives in the
immutable system image, which is the same trust the images themselves have.

**The shell lists commands from the index.** `help` prints the builtins from
the shell's own table and the executables from the index. `help COMMAND` prints
the same usage, summary, and option list as `COMMAND --help` without launching
the command.

**`userspace/utility` is a library.** It keeps the runtime hooks, startup, output,
diagnostics and filesystem helpers the commands share, and no command logic.

## Alternatives

**Generating the multicall dispatch from the directories.** Adding a command
would be as easy, the image would stay small, and the build would stay one
compilation. Every process would still carry every command, and a command could
not be installed or replaced without rebuilding the others.

**A shared runtime library.** The runtime and the helpers are repeated in every
image. Sharing them needs position-independent images, a dynamic loader and
page sharing between processes, none of which exist; TinyGo also compiles the
runtime per program rather than as a library a Go program links against. The
same need arises for Linux compatibility, where it is C libraries that matter.

**Carrying the manifest inside the native image.** PID 1 would read it from the
image being launched. That needs the `GOOSNAT` format to grow a section and
PID 1 to read images it currently never sees; the index needs neither.

**A separate manifest file per command.** A text file beside the source is
easy to read, but it is a second place to keep in step with the code. A Go
declaration is checked by the compiler, and the build can still read it without
compiling.

## Consequences

Adding a command is adding a directory (`docs/development/adding-command.md`).
The four lists are gone; the boot test checks that `/system/bin` has no
multicall image, that the index is in `/system`, and that PID 1 read it.

`pwd` no longer receives a VFS session: its manifest does not ask for one,
because it prints the working directory from its startup data. The name list
had granted it one it never used.

The images total 496 KiB instead of 62 KiB. Commands without filesystem access
are about 11 KiB, commands with it about 34 KiB, of which the VFS client is most;
it is the first candidate for reduction. A full userspace build of the twenty
commands takes about 24 seconds.

Launches got faster because each process loads and runs less code. At 100 Hz,
`uname` takes 3.6 ms instead of 5.0 ms and a three-stage `echo | readline |
readline` pipeline 9.7 ms instead of 12.5 ms. `ls /system` and `cat` are
unchanged within noise, and `ls /system/bin` is faster because it no longer
resolves hard links.

The loader still copies each image's code into every process. Mapping an
image's read-only pages shared instead would save the memory and, under TCG,
the translation each launch repeats; that is the next launch-path change.
