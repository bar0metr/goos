package linuxsockapi

import (
	"encoding/binary"
	"sync/atomic"
	"unsafe"
)

// The ring page is the second page of a session's pool. It holds two
// single-producer rings of fixed entries and the doorbell token:
//
//	0   submit head, written by the service
//	4   submit tail, written by the kernel
//	8   complete head, written by the kernel
//	12  complete tail, written by the service
//	16  doorbell token: 1 while a Doorbell notification is queued or being
//	    sent; the service stores 0 before it drains the submit ring
//	64  submit entries, RingEntries of RingEntrySize bytes
//	64 + RingEntries*RingEntrySize: complete entries
//
// The kernel produces on the submit ring: a committed TX slot (RingTX) or
// a consumed RX slot it hands back (RingRelease). The service produces on
// the complete ring: a drained TX slot (RingTXDone) or a filled RX slot
// (RingRXData). Slot ownership bounds what either ring holds to
// TXSlots+RXSlots entries, so a full ring means a broken peer.
//
// Heads and tails run free modulo 2^32. A producer writes the entry, then
// the tail; a consumer reads the tail, then the entry, then writes the head.
//
// Entry, RingEntrySize bytes:
//
//	0   kind, a Ring* constant
//	1   slot
//	2   flags, RingDestination on a datagram TX entry
//	3   reserved
//	4   length: the bytes in the slot
//	8   socket identity
//	16  slot generation, repeated by the side handing the slot back
//	20  address: a datagram's destination (TX) or source (RX data)
//	24  port
//	26  reserved, six bytes
const (
	RingEntries   = 32
	RingEntrySize = 32

	offSubmitHead   = 0
	offSubmitTail   = 4
	offCompleteHead = 8
	offCompleteTail = 12
	offDoorbell     = 16
	submitBase      = 64
	completeBase    = submitBase + RingEntries*RingEntrySize
)

// RingKind names a ring entry.
type RingKind uint8

const (
	// RingTX commits a TX slot to the service: Length bytes for Socket. A
	// datagram's entry is one whole datagram, with its destination when
	// Flags has RingDestination.
	RingTX RingKind = iota + 1
	// RingRelease hands an RX slot back: the kernel consumed it or dropped
	// it with its socket.
	RingRelease
	// RingTXDone hands a TX slot back: its bytes are in netd, or were
	// dropped with their socket or after a transport error.
	RingTXDone
	// RingRXData publishes a filled RX slot for Socket: Length stream bytes,
	// or one datagram from Address and Port.
	RingRXData
)

// RingDestination marks a datagram TX entry that names its destination.
const RingDestination = uint8(1)

// RingEntry is one decoded ring entry.
type RingEntry struct {
	Kind       RingKind
	Slot       uint8
	Flags      uint8
	Length     uint32
	Socket     uint64
	Generation uint32
	Address    [4]byte
	Port       uint16
}

// RingStatus is the outcome of a ring operation.
type RingStatus uint8

const (
	RingOK RingStatus = iota
	// RingEmpty: nothing to pop.
	RingEmpty
	// RingFull: no room to push.
	RingFull
	// RingBroken: the indices or the entry break the layout.
	RingBroken
)

// InitRings clears a ring page for a new session.
func InitRings(page []byte) { clear(page[:completeBase+RingEntries*RingEntrySize]) }

// RingBytes is the part of the ring page the layout uses.
const RingBytes = completeBase + RingEntries*RingEntrySize

// ValidEntry checks an entry against the layout: a known kind, a slot in
// range, a length the slot holds and flags the kind allows.
func ValidEntry(e *RingEntry) bool {
	switch e.Kind {
	case RingTX:
		return e.Slot < TXSlots && e.Length <= SlotSize && e.Flags&^RingDestination == 0 && e.Socket != 0
	case RingTXDone:
		return e.Slot < TXSlots && e.Length == 0 && e.Flags == 0
	case RingRelease:
		return e.Slot < RXSlots && e.Length == 0 && e.Flags == 0
	case RingRXData:
		return e.Slot < RXSlots && e.Length <= SlotSize && e.Flags == 0 && e.Socket != 0
	}
	return false
}

func push(page []byte, headOffset, tailOffset, base int, e *RingEntry) RingStatus {
	if len(page) < RingBytes || !ValidEntry(e) {
		return RingBroken
	}
	head := atomic.LoadUint32(word(page, headOffset))
	tail := atomic.LoadUint32(word(page, tailOffset))
	if tail-head > RingEntries {
		return RingBroken
	}
	if tail-head == RingEntries {
		return RingFull
	}
	p := page[base+int(tail%RingEntries)*RingEntrySize:][:RingEntrySize]
	clear(p)
	p[0], p[1], p[2] = byte(e.Kind), e.Slot, e.Flags
	binary.LittleEndian.PutUint32(p[4:], e.Length)
	binary.LittleEndian.PutUint64(p[8:], e.Socket)
	binary.LittleEndian.PutUint32(p[16:], e.Generation)
	copy(p[20:24], e.Address[:])
	binary.LittleEndian.PutUint16(p[24:], e.Port)
	atomic.StoreUint32(word(page, tailOffset), tail+1)
	return RingOK
}

func pop(page []byte, headOffset, tailOffset, base int, out *RingEntry) RingStatus {
	if len(page) < RingBytes {
		return RingBroken
	}
	head := atomic.LoadUint32(word(page, headOffset))
	tail := atomic.LoadUint32(word(page, tailOffset))
	if tail-head > RingEntries {
		return RingBroken
	}
	if tail == head {
		return RingEmpty
	}
	p := page[base+int(head%RingEntries)*RingEntrySize:][:RingEntrySize]
	e := RingEntry{Kind: RingKind(p[0]), Slot: p[1], Flags: p[2], Length: binary.LittleEndian.Uint32(p[4:]),
		Socket: binary.LittleEndian.Uint64(p[8:]), Generation: binary.LittleEndian.Uint32(p[16:]),
		Port: binary.LittleEndian.Uint16(p[24:])}
	copy(e.Address[:], p[20:24])
	if p[3] != 0 || !zero(p[26:RingEntrySize]) || !ValidEntry(&e) {
		return RingBroken
	}
	atomic.StoreUint32(word(page, headOffset), head+1)
	*out = e
	return RingOK
}

// PushSubmit and PopSubmit work the submit ring: the kernel pushes RingTX
// and RingRelease, the service pops them.
func PushSubmit(page []byte, e *RingEntry) RingStatus {
	if e.Kind != RingTX && e.Kind != RingRelease {
		return RingBroken
	}
	return push(page, offSubmitHead, offSubmitTail, submitBase, e)
}

func PopSubmit(page []byte, out *RingEntry) RingStatus {
	status := pop(page, offSubmitHead, offSubmitTail, submitBase, out)
	if status == RingOK && out.Kind != RingTX && out.Kind != RingRelease {
		return RingBroken
	}
	return status
}

// PushComplete and PopComplete work the complete ring: the service pushes
// RingTXDone and RingRXData, the kernel pops them.
func PushComplete(page []byte, e *RingEntry) RingStatus {
	if e.Kind != RingTXDone && e.Kind != RingRXData {
		return RingBroken
	}
	return push(page, offCompleteHead, offCompleteTail, completeBase, e)
}

func PopComplete(page []byte, out *RingEntry) RingStatus {
	status := pop(page, offCompleteHead, offCompleteTail, completeBase, out)
	if status == RingOK && out.Kind != RingTXDone && out.Kind != RingRXData {
		return RingBroken
	}
	return status
}

// CompletePending reports whether the complete ring has entries; the kernel
// uses it to skip a drain.
func CompletePending(page []byte) bool {
	return len(page) >= RingBytes && atomic.LoadUint32(word(page, offCompleteTail)) != atomic.LoadUint32(word(page, offCompleteHead))
}

// SubmitPending reports whether the submit ring has entries.
func SubmitPending(page []byte) bool {
	return len(page) >= RingBytes && atomic.LoadUint32(word(page, offSubmitTail)) != atomic.LoadUint32(word(page, offSubmitHead))
}

// ClaimDoorbell is the kernel's half of the doorbell rule: true when no
// Doorbell is outstanding, and the claim marks one. A send that then fails
// calls DropDoorbell.
func ClaimDoorbell(page []byte) bool {
	return atomic.CompareAndSwapUint32(word(page, offDoorbell), 0, 1)
}

// DropDoorbell withdraws a claim whose notification never went.
func DropDoorbell(page []byte) { atomic.StoreUint32(word(page, offDoorbell), 0) }

// TakeDoorbell is the service's half: it clears the mark before it drains
// the submit ring, so an entry pushed after the drain rings again.
func TakeDoorbell(page []byte) { atomic.StoreUint32(word(page, offDoorbell), 0) }

// ServiceCounters are one family's data-plane totals, which the service
// keeps in the ring page past the rings and the kernel only reads, for its
// diagnostics.
type ServiceCounters struct {
	Requests      uint64 // control requests taken
	Doorbells     uint64 // Doorbell notifications taken
	RingTX        uint64 // TX slots taken off the submit ring
	RingReleases  uint64 // RX slots handed back on the submit ring
	RingTXDone    uint64 // TX slots handed back on the complete ring
	RingRXData    uint64 // RX slots published on the complete ring
	NativeWrites  uint64 // native writes and sends issued
	NativeReads   uint64 // native reads ahead issued
	TXBytesCopied uint64 // bytes copied from TX slots to the native session
	RXBytesCopied uint64 // bytes copied from the native session to RX slots
}

// ServiceCounterCount is the fields of ServiceCounters, at
// ServiceCountersOffset of the ring page.
const (
	ServiceCounterCount   = 10
	ServiceCountersOffset = 2560
)

// StoreServiceCounters writes a family's counters into its ring page.
func StoreServiceCounters(page []byte, c *ServiceCounters) {
	values := [ServiceCounterCount]uint64{c.Requests, c.Doorbells, c.RingTX, c.RingReleases, c.RingTXDone, c.RingRXData,
		c.NativeWrites, c.NativeReads, c.TXBytesCopied, c.RXBytesCopied}
	for index := 0; index < ServiceCounterCount; index++ {
		atomic.StoreUint64((*uint64)(unsafe.Pointer(&page[ServiceCountersOffset+8*index])), values[index])
	}
}

// LoadServiceCounters reads them back.
func LoadServiceCounters(page []byte) ServiceCounters {
	var values [ServiceCounterCount]uint64
	for index := 0; index < ServiceCounterCount; index++ {
		values[index] = atomic.LoadUint64((*uint64)(unsafe.Pointer(&page[ServiceCountersOffset+8*index])))
	}
	return ServiceCounters{values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9]}
}
