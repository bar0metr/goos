//go:build goos_userspace

// Command write writes its text, or standard input, to a file.
package main

import (
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "write [--append] FILE [TEXT...]",
	Summary: "write TEXT, or standard input, to FILE",
	MinArgs: 1,
	MaxArgs: commands.Unbounded,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	var buffer [vfs.MaxProtocolData]byte
	pathIndex, appendMode := 1, false
	if argument, found := data.Arg(1); found && utility.Equal(argument, "--append") {
		pathIndex, appendMode = 2, true
	}
	path := utility.Path(data, pathIndex)
	created, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationCreate, Path: path})
	if created.Status == vfs.StatusAlreadyExists {
		created.Status = vfs.StatusOK
	}
	utility.Check(created, received, path, true)
	offset := uint64(0)
	if appendMode {
		stat, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
		utility.Check(stat, received, path, false)
		offset = stat.Size
	} else {
		truncated, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: path, Offset: 0})
		utility.Check(truncated, received, path, true)
	}
	opened, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: path})
	utility.Check(opened, received, path, true)
	if appendMode {
		seek, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationSeek, Handle: opened.Handle, Offset: offset})
		utility.Check(seek, received, path, false)
	}
	if _, inline := data.Arg(pathIndex + 1); inline {
		var text [startup.MaxVector + 1]byte
		size := 0
		for index := pathIndex + 1; ; index++ {
			argument, found := data.Arg(index)
			if !found {
				break
			}
			if size != 0 {
				text[size] = ' '
				size++
			}
			size += copy(text[size:len(text)-1], argument)
		}
		text[size] = '\n'
		for written := 0; written <= size; {
			chunk := copy(buffer[:], text[written:size+1])
			reply, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: buffer[:chunk]})
			utility.CheckWritten(reply, ok, path, chunk)
			written += chunk
		}
		utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
		utility.Mark("WRITE_PROCESS_OK\n")
		return
	}
	// Each request allocates from an arena that never frees, so rewind it per
	// chunk; otherwise a stream of a few KiB exhausts it.
	mark := utility.HeapMark()
	for {
		utility.HeapReset(mark)
		status, size := utility.ReadInput(buffer[:])
		if utility.Retry(status) {
			continue
		}
		if status == uint64(kcall.StatusPeerDied) {
			break
		}
		if status != 0 {
			utility.Fail(nil, "cannot read standard input")
		}
		reply, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: buffer[:size]})
		utility.CheckWritten(reply, ok, path, size)
	}
	utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	utility.Mark("WRITE_PROCESS_OK\n")
}
