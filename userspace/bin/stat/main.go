//go:build goos_userspace

// Command stat shows the type and size of a path. A directory's size is the
// total of the files under it, at any depth.
package main

import (
	"goos/internal/timekeeping"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "stat [-b|-k|-m|-g] PATH",
	Summary: "type/size; directory totals in KiB",
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
		printMetadata(reply)
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
	printMetadata(reply)
	utility.Mark("STAT_PROCESS_OK\n")
}

func printMetadata(reply vfs.Reply) {
	text := append(line[:0], "modified="...)
	if reply.Modified.Available {
		year, month, day, hour, minute, second := timekeeping.CivilUTC(reply.Modified.Seconds)
		text = appendPadded(text, uint64(year), 4)
		text = append(text, '-')
		text = appendPadded(text, uint64(month), 2)
		text = append(text, '-')
		text = appendPadded(text, uint64(day), 2)
		text = append(text, 'T')
		text = appendPadded(text, uint64(hour), 2)
		text = append(text, ':')
		text = appendPadded(text, uint64(minute), 2)
		text = append(text, ':')
		text = appendPadded(text, uint64(second), 2)
		text = append(text, 'Z')
	} else {
		text = append(text, "unavailable"...)
	}
	text = append(text, " attributes="...)
	if !reply.AttributesAvailable {
		text = append(text, "unavailable"...)
	} else if reply.Attributes == 0 {
		text = append(text, '-')
	} else {
		first := true
		for _, attribute := range []struct {
			flag ramfs.Attributes
			name string
		}{{ramfs.AttrDirectory, "directory"}, {ramfs.AttrReadOnly, "read-only"}, {ramfs.AttrHidden, "hidden"}, {ramfs.AttrSystem, "system"}, {ramfs.AttrArchive, "archive"}} {
			if reply.Attributes&attribute.flag == 0 {
				continue
			}
			if !first {
				text = append(text, ',')
			}
			text = append(text, attribute.name...)
			first = false
		}
	}
	text = append(text, " mount="...)
	text = utility.AppendUint(text, uint64(reply.MountID))
	utility.Print(text)
	utility.EndLine()
}

func appendPadded(target []byte, value uint64, width int) []byte {
	var digits [20]byte
	index := len(digits)
	for {
		index--
		digits[index] = byte(value%10) + '0'
		value /= 10
		if value == 0 {
			break
		}
	}
	for padding := width - (len(digits) - index); padding > 0; padding-- {
		target = append(target, '0')
	}
	return append(target, digits[index:]...)
}

// walk adds up the directory at walkPath[:length] and everything below it.
// Mounts inside it are counted as the directories they appear as.
func walk(path []byte, length int) {
	depth := 0
	levelCursor[0], levelLength[0] = 0, uint16(length)
	// The walk keeps its state in package arrays; each exchange's reply is
	// garbage once its entry is counted.
	for depth >= 0 {
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
