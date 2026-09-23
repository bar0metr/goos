//go:build goos_userspace

// Command rm removes a file or an empty directory.
package main

import (
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "rm PATH",
	Summary: "remove a file or an empty directory",
	MinArgs: 1,
	MaxArgs: 1,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	path := utility.Path(data, 1)
	reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
	utility.Check(reply, received, path, true)
	utility.Mark("RM_PROCESS_OK\n")
}
