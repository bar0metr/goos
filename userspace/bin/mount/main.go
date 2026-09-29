//go:build goos_userspace

// Command mount lists the namespace's mounted filesystems. It is deliberately
// informational and has no operation that changes the mount table.
package main

import (
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/fsdisplay"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "mount",
	Summary: "list mounted filesystems",
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(startup.Data) {
	var cursor uint32
	for {
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationReadMount, Offset: uint64(cursor)})
		if received && reply.Status == vfs.StatusNotFound {
			break
		}
		utility.Check(reply, received, nil, false)
		utility.Print(reply.Name)
		utility.Pad(len(reply.Name), 12)
		name := fsdisplay.TypeName(reply.Filesystem.Type)
		utility.PrintText(name)
		utility.Pad(len(name), 8)
		utility.PrintText(fsdisplay.Flag(reply.Filesystem.ReadOnly))
		utility.EndLine()
		cursor = reply.Cursor
	}
	utility.Mark("MOUNT_PROCESS_OK\n")
}
