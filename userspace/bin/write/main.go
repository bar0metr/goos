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
	Summary: "write TEXT or stdin to FILE",
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
		// The text is the arguments joined by spaces, however long, sent a
		// buffer at a time.
		size := 0
		for index := pathIndex + 1; ; index++ {
			argument, found := data.Arg(index)
			if !found {
				break
			}
			if index != pathIndex+1 {
				size = appendText(opened.Handle, path, &buffer, size, space[:])
			}
			size = appendText(opened.Handle, path, &buffer, size, argument)
		}
		size = appendText(opened.Handle, path, &buffer, size, newline[:])
		writeText(opened.Handle, path, buffer[:size])
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

var (
	space   = [1]byte{' '}
	newline = [1]byte{'\n'}
)

// appendText adds text to the size bytes buffered, writing each buffer that
// fills, and returns how many bytes are buffered after it.
func appendText(file vfs.Handle, path []byte, buffer *[vfs.MaxProtocolData]byte, size int, text []byte) int {
	for len(text) != 0 {
		copied := copy(buffer[size:], text)
		size += copied
		text = text[copied:]
		if size == len(buffer) {
			writeText(file, path, buffer[:size])
			size = 0
		}
	}
	return size
}

func writeText(file vfs.Handle, path []byte, text []byte) {
	if len(text) == 0 {
		return
	}
	reply, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: file, Data: text})
	utility.CheckWritten(reply, ok, path, len(text))
}
