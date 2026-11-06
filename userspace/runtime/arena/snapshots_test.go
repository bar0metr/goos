package arena

import (
	"testing"
	"unsafe"

	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// The snapshot room EnableThreads reserves goes to a kernel that takes whole
// pages only, as kernel/vmcalls.go VMMap does. A room of exactly
// snapshotBytes was refused, and no process could start a second thread.
func TestSnapshotRoomIsWholePages(t *testing.T) {
	var kept [][]byte
	sys.HostVMMap = func(address, length, protection, flags uint64) (uint64, kcall.Status) {
		if length == 0 || length%4096 != 0 {
			return 0, kcall.StatusInvalidArgument
		}
		memory := make([]byte, length+4096)
		kept = append(kept, memory)
		return (uint64(uintptr(unsafe.Pointer(&memory[0]))) + 4095) &^ 4095, kcall.StatusOK
	}
	t.Cleanup(func() { sys.HostVMMap = nil })
	room, ok := reserveSnapshots()
	if !ok || uint64(len(room)) != snapshotBytes {
		t.Fatalf("reserveSnapshots() = %d bytes, %v; want %d", len(room), ok, snapshotBytes)
	}
	if len(kept) != 1 {
		t.Fatalf("%d reservations, want 1", len(kept))
	}
}
