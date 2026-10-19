package sys

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"unsafe"
)

// Native virtual memory (ADR 0104).

// Region describes one region of the caller's address space.
type Region struct {
	Base       uint64
	Length     uint64
	Protection uint64
	Backing    uint8
	Pinned     bool
}

// VMReserve reserves length bytes of demand-zero anonymous memory with the
// given protection at an address the kernel chooses, and returns the address.
// Nothing is allocated until a page is touched.
func VMReserve(length uint64, protection uint64) (uint64, kcall.Status) {
	w := current()
	w.vmDescriptor[0], w.vmDescriptor[1] = 0, length
	result := status(vmMap(unsafe.Pointer(&w.vmDescriptor[0]), protection, 0))
	if result != kcall.StatusOK {
		return 0, result
	}
	return w.vmDescriptor[0], kcall.StatusOK
}

// VMReserveAt is VMReserve at a fixed address, which must be free.
func VMReserveAt(address uint64, length uint64, protection uint64) kcall.Status {
	w := current()
	w.vmDescriptor[0], w.vmDescriptor[1] = address, length
	return status(vmMap(unsafe.Pointer(&w.vmDescriptor[0]), protection, kcall.VMMapFixed))
}

// VMUnmap removes a range of the caller's mappings.
func VMUnmap(address uint64, length uint64) kcall.Status { return status(vmUnmap(address, length)) }

// VMProtect changes the protection of a range of anonymous memory.
func VMProtect(address uint64, length uint64, protection uint64) kcall.Status {
	return status(vmProtect(address, length, protection))
}

// VMDiscard gives the pages of a range of anonymous memory back; the range
// stays reserved and reads as zeros.
func VMDiscard(address uint64, length uint64) kcall.Status { return status(vmDiscard(address, length)) }

// VMQuery describes the region containing address.
func VMQuery(address uint64) (Region, kcall.Status) {
	w := current()
	w.vmRecord = [kcall.VMRecordSize]byte{}
	result := status(vmQuery(address, unsafe.Pointer(&w.vmRecord[0])))
	if result != kcall.StatusOK {
		return Region{}, result
	}
	return Region{Base: le64(w.vmRecord[0:8]), Length: le64(w.vmRecord[8:16]), Protection: uint64(w.vmRecord[16]),
		Backing: w.vmRecord[17], Pinned: w.vmRecord[18]&1 != 0}, kcall.StatusOK
}

// Window returns the memory of a range the caller reserved, as a slice.
func Window(address uint64, length uint64) []byte { return window(address, length) }

func le64(data []byte) uint64 {
	value := uint64(0)
	for index := 7; index >= 0; index-- {
		value = value<<8 | uint64(data[index])
	}
	return value
}

// MemoryCreate creates a shared-memory object of pages zeroed pages, charged
// to the caller, and returns a handle with Read, Write, and Transfer
// (ADR 0107).
func MemoryCreate(pages uint64) (handle.ID, kcall.Status) {
	w := current()
	w.valueWire = 0
	if result := status(memoryCreate(pages, unsafe.Pointer(&w.valueWire))); result != kcall.StatusOK {
		return 0, result
	}
	return handle.ID(w.valueWire), kcall.StatusOK
}

// HandleDuplicate derives a memory object or stream endpoint with a subset
// of its rights. The caller retains its original handle.
func HandleDuplicate(id handle.ID, rights capability.Rights) (handle.ID, kcall.Status) {
	w := current()
	w.valueWire = 0
	if result := status(handleDuplicate(uint64(id), uint64(rights), unsafe.Pointer(&w.valueWire))); result != kcall.StatusOK {
		return 0, result
	}
	return handle.ID(w.valueWire), kcall.StatusOK
}
