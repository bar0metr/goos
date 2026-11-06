package sys

import (
	"testing"
	"unsafe"

	"goos/kernel/kcall"
	"goos/userspace/runtime/tls"
)

// fakeKernel answers VMMap the way the kernel does for a reservation: it
// refuses an empty length or one that is not a whole number of pages with
// InvalidArgument (kernel/vmcalls.go VMMap), and otherwise hands out
// page-aligned host memory it keeps alive.
type fakeKernel struct {
	kept    [][]byte
	lengths []uint64
}

func (k *fakeKernel) vmMap(address, length, protection, flags uint64) (uint64, kcall.Status) {
	if length == 0 || length%4096 != 0 || flags&kcall.VMMapFixed != 0 {
		return 0, kcall.StatusInvalidArgument
	}
	memory := make([]byte, length+4096)
	k.kept = append(k.kept, memory)
	k.lengths = append(k.lengths, length)
	base := (uint64(uintptr(unsafe.Pointer(&memory[0]))) + 4095) &^ 4095
	return base, kcall.StatusOK
}

func withFakeKernel(t *testing.T) *fakeKernel {
	t.Helper()
	kernel := &fakeKernel{}
	HostVMMap = kernel.vmMap
	t.Cleanup(func() { HostVMMap = nil })
	return kernel
}

// The collector's snapshot room is 63 records of 416 bytes, 26208 bytes:
// a reservation of exactly that size is refused, so every thread start
// failed with ResourceExhausted.
func TestVMReserveBytesRoundsToPages(t *testing.T) {
	kernel := withFakeKernel(t)
	snapshots := kcall.MaxThreadSnapshots * kcall.ThreadSnapshotSize
	if _, status := VMReserve(snapshots, kcall.VMRead|kcall.VMWrite); status != kcall.StatusInvalidArgument {
		t.Fatalf("the fake kernel took %d bytes: %d", snapshots, status)
	}
	base, length, status := VMReserveBytes(snapshots, kcall.VMRead|kcall.VMWrite)
	if status != kcall.StatusOK || base == 0 || length != 28672 || base%4096 != 0 {
		t.Fatalf("VMReserveBytes(%d) = %#x, %d, %d", snapshots, base, length, status)
	}
	for _, bytes := range []uint64{1, 4095, 4096, 4097, 3 * 4096} {
		_, length, status := VMReserveBytes(bytes, kcall.VMRead)
		if status != kcall.StatusOK || length%4096 != 0 || length < bytes || length-bytes >= 4096 {
			t.Fatalf("VMReserveBytes(%d) reserved %d: %d", bytes, length, status)
		}
	}
	if _, _, status := VMReserveBytes(^uint64(0), kcall.VMRead); status != kcall.StatusInvalidArgument {
		t.Fatalf("a length that wraps was passed on: %d", status)
	}
	if len(kernel.lengths) != 6 {
		t.Fatalf("%d reservations reached the kernel, want 6", len(kernel.lengths))
	}
}

// Every slot's record lies inside the reservation, on its own cache lines,
// whatever the record's size.
func TestSlotRecordsFitTheReservation(t *testing.T) {
	kernel := withFakeKernel(t)
	for _, size := range []uintptr{1, 63, 64, 65, 416, 1000, 4096, 4100} {
		var records SlotRecords
		if records.At(1, size) != nil {
			t.Fatalf("size %d: a record before Reserve", size)
		}
		if !records.Reserve(size) || !records.Reserve(size) {
			t.Fatalf("size %d: Reserve refused", size)
		}
		base := uintptr(records.base.Load())
		length := uintptr(kernel.lengths[len(kernel.lengths)-1])
		if records.At(0, size) != nil || records.At(tls.Slots, size) != nil {
			t.Fatalf("size %d: slot 0 or a slot out of range has a record", size)
		}
		previous := uintptr(0)
		for slot := 1; slot < tls.Slots; slot++ {
			at := uintptr(records.At(slot, size))
			if at < base || at+size > base+length || at%recordAlign != 0 || previous != 0 && at-previous < size {
				t.Fatalf("size %d slot %d: record at %#x, reservation %#x+%d", size, slot, at, base, length)
			}
			previous = at
		}
	}
}
