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
	Usage:   "mkdir DIRECTORY...",
	Summary: "create directories",
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
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationMkdir, Path: path})
		utility.Check(reply, received, path, true)
	}
	utility.Mark("MKDIR_PROCESS_OK\n")
}
