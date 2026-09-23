//go:build goos_userspace

// Command stat shows the type and size of a path.
package main

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "stat PATH",
	Summary: "show the type and size of a path",
	MinArgs: 1,
	MaxArgs: 1,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	path := utility.Path(data, 1)
	reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	utility.Check(reply, received, path, false)
	if reply.Kind == ramfs.KindDirectory {
		utility.PrintText("directory ")
	} else {
		utility.PrintText("file ")
	}
	utility.PrintUint(reply.Size)
	utility.PrintText(" bytes")
	utility.EndLine()
	utility.Mark("STAT_PROCESS_OK\n")
}
