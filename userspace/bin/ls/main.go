//go:build goos_userspace

// Command ls lists a directory.
package main

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "ls [PATH]",
	Summary: "list a directory (default: the working directory)",
	MaxArgs: 1,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	argument, found := data.Arg(1)
	if !found {
		argument = []byte(".")
	}
	path := utility.Resolve(data, argument)
	stat, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	utility.Check(stat, ok, path, false)
	if stat.Kind == ramfs.KindFile {
		utility.Print(baseName(path))
		utility.EndLine()
		utility.Mark("LS_PROCESS_OK\n")
		return
	}
	var cursor uint32
	// Each exchange allocates from an arena that never frees, so it is
	// rewound per entry; a large directory would otherwise exhaust it.
	mark := utility.HeapMark()
	for {
		utility.HeapReset(mark)
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationReadDir, Path: path, Offset: uint64(cursor)})
		if received && reply.Status == vfs.StatusNotFound {
			break
		}
		utility.Check(reply, received, path, false)
		utility.Print(reply.Name)
		if reply.Kind == ramfs.KindDirectory {
			utility.PrintText("/")
		}
		utility.EndLine()
		cursor = reply.Cursor
	}
	utility.Mark("LS_PROCESS_OK\n")
}

func baseName(path []byte) []byte {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '/' {
			return path[index+1:]
		}
	}
	return path
}
