package linuxsockapi

import (
	"encoding/binary"
	"sync/atomic"
	"unsafe"
)

// The readiness pages open a session's pool. The service writes them and
// the kernel only reads them. The kernel maps the pool page by page, so an
// entry never crosses a page: the header and the entries are multiples of
// 32 bytes.
//
//	header, 64 bytes, in the first readiness page:
//	  0   magic, ReadinessMagic
//	  4   session generation
//	  8   token: 1 while a ReadyChanged notification is queued or being
//	      sent; the kernel stores 0 before it reads the entries
//	entry i at byte 64 + 32*i of the readiness pages, for i < MaxSockets:
//	  0   sequence, odd while the service rewrites the entry
//	  8   socket identity, zero for a free entry
//	  16  Ready mask
//	  20  readable bytes, or the next datagram's length
//	  24  pending error, a Result
//	  28  data-plane state, Entry* bits
//
// An entry is read by copying it between two loads of its sequence; an odd
// or changed sequence means the copy raced a write and is taken again.
const (
	ReadinessMagic   = uint32(0x4c4e5852) // "RXNL"
	headerSize       = 64
	entrySize        = 32
	offMagic         = 0
	offSession       = 4
	offToken         = 8
	offEntrySequence = 0
	offEntrySocket   = 8
	offEntryReady    = 16
	offEntryReadable = 20
	offEntryError    = 24
	offEntryState    = 28
	ReadinessBytes   = headerSize + MaxSockets*entrySize
)

// Readiness is one socket's published state. Ready and Readable describe
// what linuxsockd and netd hold, not the RX slots the kernel holds already;
// the kernel adds those.
type Readiness struct {
	Socket   uint64
	Ready    Ready
	Readable uint32
	Error    Result
	State    EntryState
}

// EntryState tells the kernel which sends it may commit to TX slots.
type EntryState uint32

const (
	// EntrySend: the socket takes sends through TX slots now: a connected
	// stream with its write side open and no pending error, or a datagram
	// socket with a port.
	EntrySend EntryState = 1 << iota
	// EntryConnected: a datagram socket has a fixed peer, so a send naming
	// an address asks with OpSendTo first.
	EntryConnected
	allStates = EntrySend | EntryConnected
)

func entryOffset(index int) int { return headerSize + index*entrySize }

// EntryPage is the pool page that holds entry index.
func EntryPage(index int) uint32 { return uint32(ReadinessPage + entryOffset(index)/pageSize) }

// Entries never straddle a page.
var _ [pageSize%entrySize + headerSize%entrySize]struct{}

func word(page []byte, offset int) *uint32 {
	return (*uint32)(unsafe.Pointer(&page[offset]))
}

// InitReadiness clears the readiness pages of a pool for a new session.
func InitReadiness(pool []byte, session uint32) {
	clear(pool[:ReadinessBytes])
	binary.LittleEndian.PutUint32(pool[offSession:], session)
	atomic.StoreUint32(word(pool, offMagic), ReadinessMagic)
}

// Publish writes one entry into the readiness pages at the start of pool.
// Only the service calls it.
func Publish(pool []byte, index int, state Readiness) {
	base := entryOffset(index)
	sequence := word(pool, base+offEntrySequence)
	start := atomic.LoadUint32(sequence) | 1
	atomic.StoreUint32(sequence, start)
	atomic.StoreUint32(word(pool, base+offEntrySocket), uint32(state.Socket))
	atomic.StoreUint32(word(pool, base+offEntrySocket+4), uint32(state.Socket>>32))
	atomic.StoreUint32(word(pool, base+offEntryReady), uint32(state.Ready&allReady))
	atomic.StoreUint32(word(pool, base+offEntryReadable), state.Readable)
	atomic.StoreUint32(word(pool, base+offEntryError), uint32(state.Error))
	atomic.StoreUint32(word(pool, base+offEntryState), uint32(state.State&allStates))
	atomic.StoreUint32(sequence, start+1)
}

// EntryStatus says how a read of an entry went.
type EntryStatus uint8

const (
	// EntrySettled: the copy is one state the service published.
	EntrySettled EntryStatus = iota
	// EntryBusy: the service was rewriting the entry on every try. Its
	// write ends with a ReadyChanged notification, so a reader treats the
	// socket as not ready yet and waits for that.
	EntryBusy
	// EntryInvalid: the index or page is wrong, or the entry holds a value
	// outside the protocol. The service is broken.
	EntryInvalid
)

// ReadEntry copies entry index between two loads of its sequence, at most
// tries times. page is the pool page EntryPage(index) names.
func ReadEntry(page []byte, index int, tries int) (Readiness, EntryStatus) {
	if index < 0 || index >= MaxSockets || len(page) < pageSize {
		return Readiness{}, EntryInvalid
	}
	base := entryOffset(index) % pageSize
	sequence := word(page, base+offEntrySequence)
	for attempt := 0; attempt < tries; attempt++ {
		before := atomic.LoadUint32(sequence)
		if before&1 != 0 {
			continue
		}
		state := Readiness{
			Socket:   uint64(atomic.LoadUint32(word(page, base+offEntrySocket))) | uint64(atomic.LoadUint32(word(page, base+offEntrySocket+4)))<<32,
			Ready:    Ready(atomic.LoadUint32(word(page, base+offEntryReady))),
			Readable: atomic.LoadUint32(word(page, base+offEntryReadable)),
			Error:    Result(atomic.LoadUint32(word(page, base+offEntryError))),
			State:    EntryState(atomic.LoadUint32(word(page, base+offEntryState))),
		}
		if atomic.LoadUint32(sequence) != before {
			continue
		}
		if state.Ready&^allReady != 0 || state.Error > lastResult || state.State&^allStates != 0 {
			return Readiness{}, EntryInvalid
		}
		return state, EntrySettled
	}
	return Readiness{}, EntryBusy
}

// EntrySequence loads entry index's sequence from page, the pool page
// EntryPage(index) names. Each write of the entry adds two to it, so a
// reader that kept an earlier value sees the entry changed. ok is false
// for an index or page outside the layout.
func EntrySequence(page []byte, index int) (sequence uint32, ok bool) {
	if index < 0 || index >= MaxSockets || len(page) < pageSize {
		return 0, false
	}
	return atomic.LoadUint32(word(page, entryOffset(index)%pageSize+offEntrySequence)), true
}

// Read is ReadEntry on a whole pool that reports only whether it got a
// settled state.
func Read(pool []byte, index int, tries int) (Readiness, bool) {
	if index < 0 || index >= MaxSockets || len(pool) < ReadinessBytes {
		return Readiness{}, false
	}
	state, status := ReadEntry(pool[EntryPage(index)*pageSize:], index, tries)
	return state, status == EntrySettled
}

// ClaimToken is the service's half of the coalescing rule: it reports true
// when no notification is outstanding, and marks one outstanding. A send
// that then fails calls DropToken.
func ClaimToken(page []byte) bool {
	return atomic.CompareAndSwapUint32(word(page, offToken), 0, 1)
}

// DropToken withdraws a claim whose notification was never queued.
func DropToken(page []byte) { atomic.StoreUint32(word(page, offToken), 0) }

// TakeToken is the kernel's half: it clears the outstanding mark before the
// kernel reads the entries, so a change made after that read draws a new
// notification.
func TakeToken(page []byte) { atomic.StoreUint32(word(page, offToken), 0) }

// Magic and Session read the page header.
func Magic(page []byte) uint32   { return atomic.LoadUint32(word(page, offMagic)) }
func Session(page []byte) uint32 { return binary.LittleEndian.Uint32(page[offSession:]) }
