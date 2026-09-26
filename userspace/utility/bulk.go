//go:build goos_userspace

package utility

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/sys"
)

// BulkSize is the memory a command moves file data through by default: one
// exchange with the VFS moves this much, where an inline request moves a few
// dozen bytes (ADR 0107).
const BulkSize = 64 << 10

// Bulk is a shared-memory object a command reads files into and writes files
// from. The command keeps its handle and its mapping; each request attaches a
// copy of the handle carrying only the rights the VFS needs to map it, which the VFS
// closes when it answers.
type Bulk struct {
	object handle.ID
	Memory []byte
}

// NewBulk creates and maps a bulk buffer of size bytes, rounded up to pages.
func NewBulk(size uint64) (Bulk, bool) {
	pages := (size + 4095) / 4096
	object, status := sys.MemoryCreate(pages)
	if status != kcall.StatusOK {
		return Bulk{}, false
	}
	base, mapped, status := sys.MemoryMapAnywhere(object, true)
	if status != kcall.StatusOK {
		sys.HandleClose(object)
		return Bulk{}, false
	}
	return Bulk{object: object, Memory: sys.Window(base, mapped)}, true
}

// ReadFile reads up to count bytes from an open file into the start of the
// buffer.
func (bulk *Bulk) ReadFile(file vfs.Handle, count int) (vfs.Reply, bool) {
	return bulk.transfer(vfs.OperationReadInto, file, count, capability.Read|capability.Write)
}

// WriteFile writes the first count bytes of the buffer to an open file.
func (bulk *Bulk) WriteFile(file vfs.Handle, count int) (vfs.Reply, bool) {
	return bulk.transfer(vfs.OperationWriteFrom, file, count, capability.Read)
}

func (bulk *Bulk) transfer(operation vfs.Operation, file vfs.Handle, count int, right capability.Rights) (vfs.Reply, bool) {
	if count < 0 || count > len(bulk.Memory) {
		return vfs.Reply{}, false
	}
	duplicate, status := sys.HandleDuplicate(bulk.object, right|capability.Transfer)
	if status != kcall.StatusOK {
		return vfs.Reply{}, false
	}
	reply, ok := callVFS(vfs.Request{Operation: operation, Handle: file, Size: uint32(count)}, duplicate)
	if !ok {
		// A copy that never reached the VFS is still ours to close.
		sys.HandleClose(duplicate)
	}
	return reply, ok
}
