//go:build goos_userspace

// Command stat shows the type and size of a path. A directory's size is the
// total of the files under it, at any depth.
package main

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "stat [-b|-k|-m|-g] PATH",
	Summary: "show a path's type and size; a directory counts its contents (KiB by default)",
	MinArgs: 1,
	MaxArgs: 2,
	Grants:  commands.GrantVFS,
}

// walkPath holds the directory being read and grows by one component per
// level. The walk is iterative: each open level keeps only its cursor and the
// length of its path, and a path of vfs.MaxPath bytes has at most
// maxDepth levels, so the walk's memory is fixed however deep the tree.
var walkPath [vfs.MaxPath]byte

const maxDepth = vfs.MaxPath/2 + 1

var levelCursor [maxDepth]uint32
var levelLength [maxDepth]uint16

var total, files, directories uint64

var line [160]byte

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	unit, index := utility.UnitKiB, 1
	if data.ArgCount() == 2 {
		flag, _ := data.Arg(1)
		selected, ok := utility.SizeUnitFlag(flag)
		if !ok {
			utility.Usage()
		}
		unit, index = selected, 2
	}
	path := utility.Path(data, index)
	reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	utility.Check(reply, received, path, false)
	if reply.Kind != ramfs.KindDirectory {
		utility.Print(utility.AppendSize(append(line[:0], "file "...), reply.Size, unit))
		utility.EndLine()
		utility.Mark("STAT_PROCESS_OK\n")
		return
	}
	length := copy(walkPath[:], path)
	walk(path, length)
	text := utility.AppendSize(append(line[:0], "directory "...), total, unit)
	text = utility.AppendCount(append(text, ", "...), files, "file", "files")
	text = utility.AppendCount(append(text, ", "...), directories, "directory", "directories")
	utility.Print(text)
	utility.EndLine()
	utility.Mark("STAT_PROCESS_OK\n")
}

// walk adds up the directory at walkPath[:length] and everything below it.
// Mounts inside it are counted as the directories they appear as.
func walk(path []byte, length int) {
	depth := 0
	levelCursor[0], levelLength[0] = 0, uint16(length)
	// The walk keeps its state in package arrays, so the arena each exchange
	// allocates from is rewound per entry; a large tree would otherwise
	// exhaust it.
	mark := utility.HeapMark()
	for depth >= 0 {
		utility.HeapReset(mark)
		current := walkPath[:levelLength[depth]]
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationReadDir, Path: current, Offset: uint64(levelCursor[depth])})
		if received && reply.Status == vfs.StatusNotFound {
			depth--
			continue
		}
		utility.Check(reply, received, current, false)
		levelCursor[depth] = reply.Cursor
		if reply.Kind != ramfs.KindDirectory {
			total += reply.Size
			files++
			continue
		}
		directories++
		child := len(current)
		if walkPath[child-1] != '/' {
			if child == len(walkPath) {
				utility.Fail(path, "path too long")
			}
			walkPath[child] = '/'
			child++
		}
		if child+len(reply.Name) > len(walkPath) || depth+1 == maxDepth {
			utility.Fail(path, "path too long")
		}
		child += copy(walkPath[child:], reply.Name)
		depth++
		levelCursor[depth], levelLength[depth] = 0, uint16(child)
	}
}
