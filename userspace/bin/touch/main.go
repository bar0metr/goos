//go:build goos_userspace

// Command touch creates an empty file if it does not exist.
package main

import (
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "touch FILE...",
	Summary: "create empty files that do not exist",
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
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationCreate, Path: path})
		if reply.Status == vfs.StatusAlreadyExists {
			reply.Status = vfs.StatusOK
		}
		utility.Check(reply, received, path, true)
	}
	utility.Mark("TOUCH_PROCESS_OK\n")
}
