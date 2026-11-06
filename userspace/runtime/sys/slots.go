package sys

import (
	"sync/atomic"
	"unsafe"

	"goos/kernel/kcall"
	"goos/userspace/runtime/tls"
)

// SlotRecords keeps one record per thread slot past the main thread's in a
// single demand-zero reservation (ADR 0203). The runtime makes the
// reservation the first time a second thread needs it. A page costs memory
// once a thread whose record lies in it touches it, so a process that runs
// four threads pays for four records, and a single-threaded one for none.
// The collector does not scan the reservation, so a record must hold no
// heap address it alone keeps alive.
type SlotRecords struct {
	base atomic.Uintptr
}

// recordAlign keeps records on cache lines of their own.
const recordAlign = 64

func recordStride(size uintptr) uintptr { return (size + recordAlign - 1) &^ (recordAlign - 1) }

// Reserve makes the reservation for records of size bytes unless it exists.
// Two threads may race; the loser unmaps its reservation. It reports false
// when the kernel refuses the reservation.
func (records *SlotRecords) Reserve(size uintptr) bool {
	if records.base.Load() != 0 {
		return true
	}
	reserved, length, result := VMReserveBytes(uint64(recordStride(size))*(tls.Slots-1), kcall.VMRead|kcall.VMWrite)
	if result != kcall.StatusOK || reserved == 0 {
		return false
	}
	if !records.base.CompareAndSwap(0, uintptr(reserved)) {
		VMUnmap(reserved, length)
	}
	return true
}

// At returns the record of slot for records of size bytes: nil for slot 0,
// a slot out of range, or before Reserve succeeded.
func (records *SlotRecords) At(slot int, size uintptr) unsafe.Pointer {
	base := records.base.Load()
	if base == 0 || slot <= 0 || slot >= tls.Slots {
		return nil
	}
	address := base + uintptr(slot-1)*recordStride(size)
	// SAFETY: the reservation holds tls.Slots-1 records of this stride, and
	// it is never unmapped once published. The address comes from the
	// kernel, not from a Go pointer, so its bits are reused, not converted.
	return *(*unsafe.Pointer)(unsafe.Pointer(&address))
}
