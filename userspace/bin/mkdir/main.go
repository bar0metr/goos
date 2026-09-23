//go:build goos_userspace

// Command mkdir creates a directory.
package main

import (
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "mkdir DIRECTORY",
	Summary: "create a directory",
	MinArgs: 1,
	MaxArgs: 1,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	path := utility.Path(data, 1)
	reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationMkdir, Path: path})
	utility.Check(reply, received, path, true)
	utility.Mark("MKDIR_PROCESS_OK\n")
}
