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
	Usage:   "rm PATH...",
	Summary: "remove file/empty directory",
	MinArgs: 1,
	MaxArgs: commands.Unbounded,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	for index := 1; index <= data.ArgCount(); index++ {
		path := utility.Path(data, index)
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
		utility.Check(reply, received, path, true)
	}
	utility.Mark("RM_PROCESS_OK\n")
}
