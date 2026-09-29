//go:build goos_userspace

package utility

import (
	"goos/internal/limits"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/runtime/arena"
	"goos/userspace/safesave"
	"goos/userspace/startup"
)

// The operations below report a failed request instead of ending the command
// the way Check and Fail do. A one-shot command stops at its first error; an
// interactive program shows the error and carries on (ADR 0124). A request
// the filesystem service never answered is reported as vfs.StatusPeerDied.

// replyName holds the name of the last reply that carried one, so what the
// exchange allocated can be freed before the caller looks at it.
var replyName [limits.NameMax]byte

// Request sends one VFS request and returns its status and reply, freeing
// what the exchange allocated, so a long-running program can make any number
// of them. The reply's Name is only valid until the next request; it carries
// no Data.
func Request(request vfs.Request) (vfs.Reply, vfs.Status) {
	mark := arena.Current()
	reply, received := CallVFS(request)
	reply.Name = replyName[:copy(replyName[:], reply.Name)]
	reply.Data = nil
	arena.Reset(mark)
	if !received {
		return vfs.Reply{}, vfs.StatusPeerDied
	}
	return reply, reply.Status
}

// Stat returns what path is and its size.
func Stat(path []byte) (ramfs.Kind, uint64, vfs.Status) {
	reply, status := Request(vfs.Request{Operation: vfs.OperationStat, Path: path})
	return reply.Kind, reply.Size, status
}

// DirVisitor receives the entries of a directory listing. Visit returns
// false to end the listing early; the name is only valid during the call.
type DirVisitor interface {
	Visit(name []byte, kind ramfs.Kind, size uint64) bool
}

// ReadDir gives visitor each entry of the directory at path, in the order the
// filesystem lists them. A listing that ends normally, or that the visitor
// ends, reports StatusOK.
func ReadDir(path []byte, visitor DirVisitor) vfs.Status {
	var cursor uint32
	for {
		reply, status := Request(vfs.Request{Operation: vfs.OperationReadDir, Path: path, Offset: uint64(cursor)})
		if status == vfs.StatusNotFound && cursor != 0 {
			return vfs.StatusOK
		}
		if status == vfs.StatusNotFound {
			// An empty directory answers its first read the same way a missing
			// one does; Stat tells them apart.
			if _, _, statStatus := Stat(path); statStatus != vfs.StatusOK {
				return statStatus
			}
			return vfs.StatusOK
		}
		if status != vfs.StatusOK {
			return status
		}
		if !visitor.Visit(reply.Name, reply.Kind, reply.Size) {
			return vfs.StatusOK
		}
		cursor = reply.Cursor
	}
}

// Mkdir creates a directory.
func Mkdir(path []byte) vfs.Status {
	_, status := Request(vfs.Request{Operation: vfs.OperationMkdir, Path: path})
	return status
}

// CreateFile creates an empty regular file.
func CreateFile(path []byte) vfs.Status {
	_, status := Request(vfs.Request{Operation: vfs.OperationCreate, Path: path})
	return status
}

// Rename moves source to target within one filesystem. Across filesystems
// the VFS answers StatusUnsupported.
func Rename(source, target []byte) vfs.Status {
	_, status := Request(vfs.Request{Operation: vfs.OperationRename, Path: source, Target: target})
	return status
}

// Replace moves the regular file source onto target within one filesystem,
// removing the file target named, or renames it when there was none
// (ADR 0129).
func Replace(source, target []byte) vfs.Status {
	_, status := Request(vfs.Request{Operation: vfs.OperationReplace, Path: source, Target: target})
	return status
}

// savePath holds the temporary path of the save in progress.
var savePath [limits.PathMax]byte

// SaveFile replaces target with what source produces, writing it through bulk
// to a temporary file beside target first, so target keeps its old contents
// until the new ones are complete (ADR 0129).
func SaveFile(target []byte, bulk *Bulk, source safesave.Source) (vfs.Status, safesave.Stage) {
	return safesave.Save(saveFiles{bulk}, target, bulk.Memory, source, &savePath)
}

// saveFiles is the VFS a save goes through. Save fills bulk's memory, so a
// write sends a prefix of it.
type saveFiles struct{ bulk *Bulk }

func (saveFiles) Create(path []byte) vfs.Status { return CreateFile(path) }
func (saveFiles) Unlink(path []byte) vfs.Status { return Unlink(path) }
func (saveFiles) Replace(source, target []byte) vfs.Status {
	return Replace(source, target)
}

func (saveFiles) OpenWrite(path []byte) (vfs.Handle, vfs.Status) {
	reply, status := Request(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: path})
	return reply.Handle, status
}

func (saveFiles) Close(file vfs.Handle) vfs.Status {
	_, status := Request(vfs.Request{Operation: vfs.OperationClose, Handle: file})
	return status
}

func (files saveFiles) Write(file vfs.Handle, data []byte) (int, vfs.Status) {
	mark := arena.Current()
	reply, received := files.bulk.WriteFile(file, len(data))
	arena.Reset(mark)
	if !received {
		return 0, vfs.StatusPeerDied
	}
	return int(reply.Size), reply.Status
}

// Unlink removes a file or an empty directory.
func Unlink(path []byte) vfs.Status {
	_, status := Request(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
	return status
}

// CopyFailure says which side of a copy failed.
type CopyFailure uint8

const (
	CopySource CopyFailure = iota + 1
	CopyTarget
)

// CopyFile copies the regular file source to target through bulk, one
// buffer at a time, so a file of any size needs no more memory than the
// buffer. An existing target is replaced only when overwrite is set;
// otherwise it answers StatusAlreadyExists and is left alone. A short write
// is reported as StatusNoSpace. Both files are closed on every path, and the
// source is never written.
func CopyFile(source, target []byte, bulk *Bulk, overwrite bool) (vfs.Status, CopyFailure) {
	if sameBytes(source, target) {
		// Truncating the target would empty the source.
		return vfs.StatusInvalidArgument, CopyTarget
	}
	input, status := Request(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: source})
	if status != vfs.StatusOK {
		return status, CopySource
	}
	status, failure := copyInto(input.Handle, target, bulk, overwrite)
	Request(vfs.Request{Operation: vfs.OperationClose, Handle: input.Handle})
	return status, failure
}

func copyInto(input vfs.Handle, target []byte, bulk *Bulk, overwrite bool) (vfs.Status, CopyFailure) {
	status := CreateFile(target)
	if status == vfs.StatusAlreadyExists && overwrite {
		status = vfs.StatusOK
	}
	if status != vfs.StatusOK {
		return status, CopyTarget
	}
	if _, status = Request(vfs.Request{Operation: vfs.OperationTruncate, Path: target, Offset: 0}); status != vfs.StatusOK {
		return status, CopyTarget
	}
	output, status := Request(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: target})
	if status != vfs.StatusOK {
		return status, CopyTarget
	}
	status, failure := transfer(input, output.Handle, bulk)
	Request(vfs.Request{Operation: vfs.OperationClose, Handle: output.Handle})
	return status, failure
}

func transfer(input, output vfs.Handle, bulk *Bulk) (vfs.Status, CopyFailure) {
	for {
		mark := arena.Current()
		chunk, received := bulk.ReadFile(input, len(bulk.Memory))
		arena.Reset(mark)
		if !received {
			return vfs.StatusPeerDied, CopySource
		}
		if chunk.Status != vfs.StatusOK {
			return chunk.Status, CopySource
		}
		if chunk.Size == 0 {
			return vfs.StatusOK, 0
		}
		written, received := bulk.WriteFile(output, int(chunk.Size))
		arena.Reset(mark)
		if !received {
			return vfs.StatusPeerDied, CopyTarget
		}
		if written.Status != vfs.StatusOK {
			return written.Status, CopyTarget
		}
		if written.Size != chunk.Size {
			return vfs.StatusNoSpace, CopyTarget
		}
	}
}

func sameBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// TerminalSize returns the size of the framebuffer console as the shell
// passes it in COLUMNS and LINES, and whether it did: a serial-only console
// has no size of its own, and a full-screen program asks the terminal at its
// far end instead (terminal.Output.RequestSize).
func TerminalSize(data startup.Data) (int, int, bool) {
	columnsText, columnsFound := data.Env("COLUMNS")
	linesText, linesFound := data.Env("LINES")
	if !columnsFound || !linesFound {
		return 0, 0, false
	}
	columns, columnsOK := parseUint(columnsText)
	rows, rowsOK := parseUint(linesText)
	if !columnsOK || !rowsOK || columns == 0 || rows == 0 || columns > 0xffff || rows > 0xffff {
		return 0, 0, false
	}
	return int(columns), int(rows), true
}
