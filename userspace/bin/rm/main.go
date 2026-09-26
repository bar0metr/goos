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
	Summary: "remove files or empty directories",
	MinArgs: 1,
	MaxArgs: commands.Unbounded,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	mark := utility.HeapMark()
	for index := 1; index <= data.ArgCount(); index++ {
		utility.HeapReset(mark)
		path := utility.Path(data, index)
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
		utility.Check(reply, received, path, true)
	}
	utility.Mark("RM_PROCESS_OK\n")
}
