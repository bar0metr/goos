//go:build goos_userspace

package utility

import (
	"goos/internal/capability"
	"goos/internal/limits"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/sys"
)

// BulkSize is the default transfer size (ADR 0107).
const BulkSize = 64 << 10

// Bulk has caller-owned Memory and a separate shared staging object. Successful
// reads copy into Memory before returning; writes snapshot it before sending.
// After an abandoned transfer the staging object is retired, so late server
// access cannot race the caller's Memory. This costs one additional buffer and
// one copy per transfer. A Bulk belongs to one goroutine and must not be copied
// after use. Different Bulks take turns on the filesystem session.
type Bulk struct {
	bulkBuffer[nativeBulk]
	temporary *[limits.PathMax]byte
}

// NewBulk allocates size bytes rounded to pages, plus shared staging storage.
func NewBulk(size uint64) (Bulk, bool) {
	if size == 0 || size > uint64(^uint32(0))-4095 {
		return Bulk{}, false
	}
	size = (size + 4095) / 4096 * 4096
	var bulk Bulk
	if !bulk.ensure(size) {
		return Bulk{}, false
	}
	bulk.Memory = make([]byte, int(size))
	return bulk, true
}

func (bulk *Bulk) ReadFile(file vfs.Handle, count int) (vfs.Reply, bool) {
	return bulk.transfer(vfs.OperationReadInto, file, count)
}

func (bulk *Bulk) WriteFile(file vfs.Handle, count int) (vfs.Reply, bool) {
	return bulk.transfer(vfs.OperationWriteFrom, file, count)
}

type nativeBulk struct{}

func (nativeBulk) create(size uint64) (handle.ID, uint64, []byte, bool) {
	object, status := sys.MemoryCreate((size + 4095) / 4096)
	if status != kcall.StatusOK {
		return 0, 0, nil, false
	}
	base, mapped, status := sys.MemoryMapAnywhere(object, true)
	if status != kcall.StatusOK {
		sys.HandleClose(object)
		return 0, 0, nil, false
	}
	return object, base, sys.Window(base, mapped), true
}

func (nativeBulk) duplicate(object handle.ID, rights capability.Rights) (handle.ID, bool) {
	attached, status := sys.HandleDuplicate(object, rights)
	return attached, status == kcall.StatusOK
}

func (nativeBulk) retire(object handle.ID, base, size uint64) {
	sys.VMUnmap(base, size)
	sys.HandleClose(object)
}

func (nativeBulk) call(request vfs.Request, attached handle.ID) (vfs.Reply, bool) {
	return attachedVFS(request, attached)
}
