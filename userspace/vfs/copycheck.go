package vfsservice

import (
	"goos/internal/validation"
	"unsafe"
)

// CopyFault describes a read-path copy that a checked build refused to run.
// A slice with a nil array and a nonzero length cannot come from safe Go; it
// means a torn slice header or corrupted state, and the copy would fault in
// memmove without saying which buffer it read.
type CopyFault struct {
	// Op names the check: dispatch, execute, collect, deliver, reply or
	// cache.
	Op       string
	Src, Dst uintptr
	Len      int
	Session  ReadSession
	// Current is the session's current generation, or zero where the check
	// cannot see the session table.
	Current uint64
	// Ticket is the read plan's pool ticket, or zero outside the pool.
	Ticket uint64
	State  string
	// Worker is the pool worker index, CopyWorkerOwner or CopyWorkerUnknown.
	Worker int
}

// Worker values outside the pool's worker indexes.
const (
	CopyWorkerOwner   = -1
	CopyWorkerUnknown = -2
)

// CopyFaultHandler receives a refused copy. It must not return: the caller's
// state is no longer trustworthy. A nil handler disables the checks.
type CopyFaultHandler func(CopyFault)

// SetCopyFault enables the read-path copy checks. The caller sets it once,
// before NewReadPool starts workers and before any read, so workers read an
// immutable handler.
func (service *Service) SetCopyFault(handler CopyFaultHandler) {
	if validation.Enabled {
		service.copyFault = handler
		service.persistent.copyFault = handler
	}
}

// SetCopyContext labels owner-side reads at a drained serial barrier.
// Workers replace the context only while they own the persistent lane.
func (service *Service) SetCopyContext(session ReadSession, ticket uint64) {
	if validation.Enabled {
		service.persistent.copyContext = CopyFault{Session: session, Current: session.Generation, Ticket: ticket, State: "serial", Worker: CopyWorkerOwner}
	}
}

// sliceAddress is the array address of a slice, zero for a nil array.
func sliceAddress(data []byte) uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(data))) }

// copyValid reports whether copy(dst, src) can run: when it moves bytes,
// both sides have an array.
func copyValid(dst, src []byte) bool {
	return min(len(dst), len(src)) == 0 || sliceAddress(dst) != 0 && sliceAddress(src) != 0
}

// bufferValid reports whether a buffer with bytes has an array.
func bufferValid(data []byte) bool { return len(data) == 0 || sliceAddress(data) != 0 }

// AppendMarker formats the fault as one console line:
//
//	VFS_COPY_INVALID op=... src=0x... dst=0x... len=... session=... gen=...
//	cur_gen=... plan=... state=... worker=...
func (fault CopyFault) AppendMarker(line []byte) []byte {
	line = append(line, "VFS_COPY_INVALID op="...)
	line = append(line, fault.Op...)
	line = append(line, " src=0x"...)
	line = appendHex(line, uint64(fault.Src))
	line = append(line, " dst=0x"...)
	line = appendHex(line, uint64(fault.Dst))
	line = append(line, " len="...)
	if fault.Len < 0 {
		line = append(line, '-')
		line = appendDecimal(line, uint64(-fault.Len))
	} else {
		line = appendDecimal(line, uint64(fault.Len))
	}
	line = append(line, " session="...)
	line = appendDecimal(line, uint64(fault.Session.Owner))
	line = append(line, " gen="...)
	line = appendDecimal(line, fault.Session.Generation)
	line = append(line, " cur_gen="...)
	line = appendDecimal(line, fault.Current)
	line = append(line, " plan="...)
	line = appendDecimal(line, fault.Ticket)
	line = append(line, " state="...)
	line = append(line, fault.State...)
	line = append(line, " worker="...)
	switch {
	case fault.Worker == CopyWorkerOwner:
		line = append(line, "owner"...)
	case fault.Worker < 0:
		line = append(line, "unknown"...)
	default:
		line = appendDecimal(line, uint64(fault.Worker))
	}
	return append(line, '\n')
}

func appendDecimal(line []byte, value uint64) []byte {
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	return append(line, digits[position:]...)
}

func appendHex(line []byte, value uint64) []byte {
	const hex = "0123456789abcdef"
	var digits [16]byte
	position := len(digits)
	for {
		position--
		digits[position] = hex[value&15]
		value >>= 4
		if value == 0 {
			break
		}
	}
	return append(line, digits[position:]...)
}
