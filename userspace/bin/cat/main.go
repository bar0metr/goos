//go:build goos_userspace

// Command cat prints a file.
package main

import (
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "cat FILE",
	Summary: "print a file",
	MinArgs: 1,
	MaxArgs: 1,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	path := utility.Path(data, 1)
	opened, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	utility.Check(opened, received, path, false)
	for {
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		utility.Check(reply, received, path, false)
		if len(reply.Data) == 0 {
			break
		}
		utility.Write(reply.Data)
	}
	utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	utility.Mark("CAT_PROCESS_OK\n")
}
