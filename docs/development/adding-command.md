# Adding a command

A command is a program in `/system/bin` that the shell runs by name. Each one is
its own directory under `userspace/bin/` and its own image; nothing else lists
it. The build finds the directory, compiles it, installs the image as
`/system/bin/<name>` on the disk, and adds the command's manifest to the
command index `/system/commands` that PID 1 and the shell read (ADR 0080,
ADR 0175). A command that recovery needs is also added to the archive's
`RecoveryCommands` list in `tools/initramfs`, with the reason it is needed;
the archive's size limit is shared by everything in it.

For a long-running supervised program, see [Adding a native service](adding-service.md).

## 1. Create the directory

The directory name is the command name: lower-case letters, digits and `-`,
starting with a letter, at most 32 bytes.

```text
userspace/bin/hello/
└── main.go
```

## 2. Write the program

```go
//go:build goos_userspace

// Command hello greets someone.
package main

import (
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "hello [NAME]",
	Summary: "print a greeting",
	MaxArgs: 1,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	name, found := data.Arg(1)
	if !found {
		name = []byte("world")
	}
	utility.PrintText("hello, ")
	utility.Print(name)
	utility.EndLine()
}
```

That is all. `make` builds it, `help` lists it, `help hello` and
`hello --help` print its usage, and `hello a b` answers `usage: hello [NAME]`
without running `run`.

## 3. The manifest

`manifest` is the only description of the command. The build reads it from the
source without compiling it, so every field must be a literal:

| Field | Meaning |
| --- | --- |
| `Usage` | Synopsis; must start with the command name. Printable ASCII, at most 96 bytes. |
| `Summary` | One line for `help`. Printable ASCII, at most 96 bytes. |
| `MinArgs`, `MaxArgs` | Arguments after the name. `commands.Unbounded` for no limit. |
| `Grants` | `commands.GrantVFS` if the command uses the filesystem, `commands.GrantTerminal` if it is full-screen, `commands.GrantLaunch` if it starts other full-screen commands; join several with `\|`; omit otherwise. |

`Grants` is a request, not authority. PID 1 grants only roles it knows, and
only to a command launched from the origin of the index that describes it:
`/system/bin` from the protected installed system in normal mode, or
`/bootstrap/bin` from the archive. A copy of the image anywhere else gets
none (ADR 0175). A command without `GrantVFS` gets its
standard streams and nothing else, which is what most commands need: `pwd`
reads its working directory from the startup data and has no filesystem
session.

The shell reads `GrantTerminal`: it runs such a command in the
foreground with the terminal in raw mode, so standard input carries each key's
bytes as typed and nothing is echoed ([ADR 0124](../adr/0124-foreground-raw-terminal-mode-and-tui-input.md)).
It carries no handle. Such a command draws with `userspace/terminal` or
`userspace/tui`, parses keys with its `Parser`, and reads its size with
`utility.TerminalSize`. PID 1 reads it too: only a command that declares it may
be started by another application.

`GrantLaunch` gives the command a launch channel to PID 1 when it runs as the
only stage of a foreground job ([ADR 0130](../adr/0130-application-launch-and-nested-foreground.md)).
`utility.Launch` runs a `GrantTerminal` command of `/system/bin` on the
caller's terminal and returns its exit code; the caller must not read input
until it returns. `gfm` uses it for F3 and F4. A command started this way
never gets `GrantLaunch` itself.

A malformed manifest fails the build with the command's name and the reason.

## 4. What `utility` provides

`utility.Run` binds the standard streams, answers `-h` and `--help`, checks the
argument count, requires the VFS session when the manifest asks for one, calls
`run`, and exits with status 0.

| Need | Use |
| --- | --- |
| Print a line | `Print`, `PrintText`, `PrintUint`, `Pad`, then `EndLine` |
| Raw output | `Write` |
| Fail | `Fail(path, reason)` prints `name: path: reason` and exits 2; `Usage()` prints the synopsis and exits 2 |
| Paths | `Path(data, i)`, `Target(data, i)`, `Resolve(data, arg)` |
| Filesystem | `CallVFS(request)` then `Check(reply, ok, path, mutation)` |
| Input | `ReadInput`, `ReadByte`, `Retry(status)` |
| Time and system | `Clock`, `ClockNanos`, `SystemInfo`, `ProcessInfo` |
| Validation marker | `Mark("NAME_PROCESS_OK\n")`, printed only in validation builds |

Keep the startup path short. Under QEMU's TCG every launch translates the code
it runs afresh, so code a command touches before doing its work is paid for on
every run (ADR 0079).

## 5. Tests

Pure logic belongs in a host-testable package with `go test`. For behaviour in
the running system, add the command to `tests/boot/qemu_boot.sh`: type it in the
command script and check its output with `require_regex`.

Removing the directory removes the command: the next build drops its image and
its index entry.
