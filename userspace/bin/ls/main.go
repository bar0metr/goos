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
	Summary: "list directory, default CWD",
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
		utility.PrintName(baseName(path))
		utility.EndLine()
		utility.Mark("LS_PROCESS_OK\n")
		return
	}
	var cursor uint32
	var record ramfs.DirRecord
	for {
		reply, received := utility.ReadDirBatch(path, cursor, batch[:])
		if received && reply.Status == vfs.StatusNotFound {
			break
		}
		utility.Check(reply, received, path, false)
		for records := batch[:reply.Size]; len(records) != 0; {
			size, valid := record.Decode(records)
			if !valid {
				utility.Fail(path, "malformed directory listing")
			}
			records = records[size:]
			utility.PrintName(record.Name)
			if record.Stat.Kind == ramfs.KindDirectory {
				utility.PrintText("/")
			}
			utility.EndLine()
		}
		if reply.BatchEnd {
			break
		}
		cursor = reply.Cursor
	}
	utility.Mark("LS_PROCESS_OK\n")
}

// batch is the listing's record buffer: ls scans one directory at a time.
var batch [vfs.MaxDirBatch]byte

func baseName(path []byte) []byte {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '/' {
			return path[index+1:]
		}
	}
	return path
}
