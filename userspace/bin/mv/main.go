//go:build goos_userspace

// Command mv renames or moves a file or directory.
package main

import (
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "mv SOURCE TARGET",
	Summary: "move file/directory",
	MinArgs: 2,
	MaxArgs: 2,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	source := utility.Path(data, 1)
	target := utility.Target(data, 2)
	reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationRename, Path: source, Target: target})
	if reply.Status == vfs.StatusAlreadyExists {
		utility.Check(reply, received, target, true)
	}
	utility.Check(reply, received, source, true)
	utility.Mark("MV_PROCESS_OK\n")
}
