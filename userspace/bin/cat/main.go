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
	Usage:   "cat FILE...",
	Summary: "print files",
	MinArgs: 1,
	MaxArgs: commands.Unbounded,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	for index := 1; index <= data.ArgCount(); index++ {
		show(utility.Path(data, index))
	}
	utility.Mark("CAT_PROCESS_OK\n")
}

// show writes one file to standard output.
func show(path []byte) {
	opened, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	utility.Check(opened, received, path, false)
	// Each request allocates from an arena that never frees, so rewind it per
	// chunk; otherwise a file of a few KiB exhausts it.
	mark := utility.HeapMark()
	for {
		utility.HeapReset(mark)
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		utility.Check(reply, received, path, false)
		if len(reply.Data) == 0 {
			break
		}
		utility.Write(reply.Data)
	}
	utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
}
