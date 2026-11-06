package arena

import (
	"unsafe"

	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// snapshotBytes is the room a collection gives ThreadSuspend: one record for
// every slot but the collector's.
const snapshotBytes = kcall.MaxThreadSnapshots * kcall.ThreadSnapshotSize

// reserveSnapshots reserves the snapshot room outside the globals. 63
// records of 416 bytes are not a whole number of pages, so it asks for
// whole pages; the kernel refuses any other length.
func reserveSnapshots() ([]byte, bool) {
	base, _, status := sys.VMReserveBytes(snapshotBytes, kcall.VMRead|kcall.VMWrite)
	if status != kcall.StatusOK || base == 0 {
		return nil, false
	}
	// SAFETY: the reservation holds at least snapshotBytes and is never
	// unmapped.
	return unsafe.Slice((*byte)(pointerAt(uintptr(base))), snapshotBytes), true
}
