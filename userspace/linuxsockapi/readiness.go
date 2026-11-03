package linuxsockapi

import (
	"encoding/binary"
	"sync/atomic"
	"unsafe"
)

// The readiness page is the first page of a session's pool. The service
// writes it and the kernel only reads it.
//
//	header, 64 bytes:
//	  0   magic, ReadinessMagic
//	  4   session generation
//	  8   token: 1 while a ReadyChanged notification is queued or being
//	      sent; the kernel stores 0 before it reads the entries
//	entry i at 64 + 32*i, for i < MaxSockets:
//	  0   sequence, odd while the service rewrites the entry
//	  8   socket identity, zero for a free entry
//	  16  Ready mask
//	  20  readable bytes, or the next datagram's length
//	  24  pending error, a Result
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
	ReadinessBytes   = headerSize + MaxSockets*entrySize
)

// Readiness is one socket's published state.
type Readiness struct {
	Socket   uint64
	Ready    Ready
	Readable uint32
	Error    Result
}

func entryOffset(index int) int { return headerSize + index*entrySize }

func word(page []byte, offset int) *uint32 {
	return (*uint32)(unsafe.Pointer(&page[offset]))
}

// InitReadiness clears a readiness page for a new session.
func InitReadiness(page []byte, session uint32) {
	clear(page[:ReadinessBytes])
	binary.LittleEndian.PutUint32(page[offSession:], session)
	atomic.StoreUint32(word(page, offMagic), ReadinessMagic)
}

// Publish writes one entry. Only the service calls it.
func Publish(page []byte, index int, state Readiness) {
	base := entryOffset(index)
	sequence := word(page, base+offEntrySequence)
	start := atomic.LoadUint32(sequence) | 1
	atomic.StoreUint32(sequence, start)
	atomic.StoreUint32(word(page, base+offEntrySocket), uint32(state.Socket))
	atomic.StoreUint32(word(page, base+offEntrySocket+4), uint32(state.Socket>>32))
	atomic.StoreUint32(word(page, base+offEntryReady), uint32(state.Ready&allReady))
	atomic.StoreUint32(word(page, base+offEntryReadable), state.Readable)
	atomic.StoreUint32(word(page, base+offEntryError), uint32(state.Error))
	atomic.StoreUint32(sequence, start+1)
}

// Read copies entry index. It reports false when the entry raced a writer
// tries times in a row, or holds a value outside the protocol: a service
// that never settles an entry is treated as a broken peer.
func Read(page []byte, index int, tries int) (Readiness, bool) {
	if index < 0 || index >= MaxSockets || len(page) < ReadinessBytes {
		return Readiness{}, false
	}
	base := entryOffset(index)
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
		}
		if atomic.LoadUint32(sequence) != before {
			continue
		}
		if state.Ready&^allReady != 0 || state.Error > lastResult {
			return Readiness{}, false
		}
		return state, true
	}
	return Readiness{}, false
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
