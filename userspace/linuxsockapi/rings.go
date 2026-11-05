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
	WarmHits      uint64 // opens answered from the endpoints opened ahead
	WarmMisses    uint64 // opens that went to netd
	AcceptHits    uint64 // accepts answered from children accepted ahead
	AcceptMisses  uint64 // accepts that went to netd
	Refreshes     uint64 // readiness refreshes issued
	WaitsArmed    uint64 // readiness waits held in netd
	AcceptsAhead  uint64 // accepts ahead issued
	Completions   uint64 // native answers taken
}

// ServiceCounterCount is the fields of ServiceCounters, at
// ServiceCountersOffset of the ring page.
const (
	ServiceCounterCount   = 18
	ServiceCountersOffset = 2560
)

// StoreServiceCounters writes a family's counters into its ring page.
func StoreServiceCounters(page []byte, c *ServiceCounters) {
	values := [ServiceCounterCount]uint64{c.Requests, c.Doorbells, c.RingTX, c.RingReleases, c.RingTXDone, c.RingRXData,
		c.NativeWrites, c.NativeReads, c.TXBytesCopied, c.RXBytesCopied, c.WarmHits, c.WarmMisses, c.AcceptHits, c.AcceptMisses,
		c.Refreshes, c.WaitsArmed, c.AcceptsAhead, c.Completions}
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
	return ServiceCounters{values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9],
		values[10], values[11], values[12], values[13], values[14], values[15], values[16], values[17]}
}

// The service keeps a state snapshot for the kernel's diagnostics. One
// FamilySnapshot follows the counters in the ring page. The snapshot pages
// hold one SocketSnapshot per family socket index, SnapshotsPerPage to a
// page so none crosses a page. The service rewrites a socket's whenever it
// publishes or moves the socket's data plane; the kernel only reads it.
const (
	FamilySnapshotOffset = ServiceCountersOffset + 8*ServiceCounterCount
	familySnapshotSize   = 32
	SnapshotEnd          = FamilySnapshotOffset + familySnapshotSize
	socketSnapshotSize   = 40
	SnapshotsPerPage     = pageSize / socketSnapshotSize
)

// FamilySnapshot is the service's view of a family's slots and requests,
// of its native TCP link, and of the socket quotas. Sockets and
// FamilyLimit are the family's own use and quota; Reserve counts the
// sockets no other family may take from it. SystemSockets and SystemLimit
// are the service's use and quota across families as of the family's last
// change.
type FamilySnapshot struct {
	TXHeld, RXUsed, RXPublished uint8
	Inflight, Parked, Replies   uint8
	LinkTXLent, LinkReleases    uint8
	LinkBlocked                 bool
	Sockets, FamilyLimit        uint16
	Reserve                     uint16
	SystemSockets, SystemLimit  uint16
}

// SocketSnapshotPage is the pool page that holds socket index's snapshot.
func SocketSnapshotPage(index int) uint32 { return uint32(SnapshotPage + index/SnapshotsPerPage) }

// SocketSnapshot is the service's state of one socket.
type SocketSnapshot struct {
	Socket     uint64
	Endpoint   uint32
	State      uint8
	TXQueue    uint8
	WritesOut  uint8
	ReadsOut   uint8
	Flags      uint32
	TXAccepted uint32
	TXSent     uint32
	TXStream   uint32
	Mask       uint32
	Readable   uint32
}

// Socket snapshot flags.
const (
	SnapTXBlocked uint32 = 1 << iota
	SnapTXStall
	SnapTXDropped
	SnapWantTX
	SnapWantRX
	SnapWaiting
	SnapNeedWait
	SnapNeedRefresh
	SnapRefreshing
	SnapEOF
	SnapClosing
	SnapDead
	SnapReadShut
	SnapWriteShut
	SnapShutQueued
	SnapPending
)

// StoreFamilySnapshot and StoreSocketSnapshot write the snapshot; the
// Load functions read it back.
func StoreFamilySnapshot(page []byte, f *FamilySnapshot) {
	p := page[FamilySnapshotOffset : FamilySnapshotOffset+familySnapshotSize]
	p[0], p[1], p[2], p[3], p[4], p[5], p[6], p[7] = f.TXHeld, f.RXUsed, f.RXPublished, f.Inflight, f.Parked, f.Replies, f.LinkTXLent, f.LinkReleases
	p[8] = 0
	if f.LinkBlocked {
		p[8] = 1
	}
	binary.LittleEndian.PutUint16(p[10:], f.Sockets)
	binary.LittleEndian.PutUint16(p[12:], f.FamilyLimit)
	binary.LittleEndian.PutUint16(p[14:], f.Reserve)
	binary.LittleEndian.PutUint16(p[16:], f.SystemSockets)
	binary.LittleEndian.PutUint16(p[18:], f.SystemLimit)
}

func LoadFamilySnapshot(page []byte) FamilySnapshot {
	p := page[FamilySnapshotOffset : FamilySnapshotOffset+familySnapshotSize]
	return FamilySnapshot{TXHeld: p[0], RXUsed: p[1], RXPublished: p[2], Inflight: p[3], Parked: p[4], Replies: p[5],
		LinkTXLent: p[6], LinkReleases: p[7], LinkBlocked: p[8] != 0,
		Sockets: binary.LittleEndian.Uint16(p[10:]), FamilyLimit: binary.LittleEndian.Uint16(p[12:]),
		Reserve: binary.LittleEndian.Uint16(p[14:]), SystemSockets: binary.LittleEndian.Uint16(p[16:]),
		SystemLimit: binary.LittleEndian.Uint16(p[18:])}
}

// StoreSocketSnapshot writes socket index's snapshot into page, the pool
// page SocketSnapshotPage(index) names. LoadSocketSnapshot reads it back.
func StoreSocketSnapshot(page []byte, index int, s *SocketSnapshot) {
	if index < 0 || index >= MaxSockets || len(page) < pageSize {
		return
	}
	p := page[index%SnapshotsPerPage*socketSnapshotSize:][:socketSnapshotSize]
	binary.LittleEndian.PutUint64(p[0:], s.Socket)
	binary.LittleEndian.PutUint32(p[8:], s.Endpoint)
	p[12], p[13], p[14], p[15] = s.State, s.TXQueue, s.WritesOut, s.ReadsOut
	binary.LittleEndian.PutUint32(p[16:], s.Flags)
	binary.LittleEndian.PutUint32(p[20:], s.TXAccepted)
	binary.LittleEndian.PutUint32(p[24:], s.TXSent)
	binary.LittleEndian.PutUint32(p[28:], s.TXStream)
	binary.LittleEndian.PutUint32(p[32:], s.Mask)
	binary.LittleEndian.PutUint32(p[36:], s.Readable)
}

func LoadSocketSnapshot(page []byte, index int) SocketSnapshot {
	if index < 0 || index >= MaxSockets || len(page) < pageSize {
		return SocketSnapshot{}
	}
	p := page[index%SnapshotsPerPage*socketSnapshotSize:][:socketSnapshotSize]
	return SocketSnapshot{Socket: binary.LittleEndian.Uint64(p[0:]), Endpoint: binary.LittleEndian.Uint32(p[8:]),
		State: p[12], TXQueue: p[13], WritesOut: p[14], ReadsOut: p[15], Flags: binary.LittleEndian.Uint32(p[16:]),
		TXAccepted: binary.LittleEndian.Uint32(p[20:]), TXSent: binary.LittleEndian.Uint32(p[24:]),
		TXStream: binary.LittleEndian.Uint32(p[28:]), Mask: binary.LittleEndian.Uint32(p[32:]), Readable: binary.LittleEndian.Uint32(p[36:])}
}

// RingPositions reads the four ring indices and the doorbell token.
func RingPositions(page []byte) (submitHead, submitTail, completeHead, completeTail, doorbell uint32) {
	return atomic.LoadUint32(word(page, offSubmitHead)), atomic.LoadUint32(word(page, offSubmitTail)),
		atomic.LoadUint32(word(page, offCompleteHead)), atomic.LoadUint32(word(page, offCompleteTail)),
		atomic.LoadUint32(word(page, offDoorbell))
}

// Token reads the readiness page's notification token.
func Token(page []byte) uint32 { return atomic.LoadUint32(word(page, offToken)) }
