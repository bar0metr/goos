package udpapi

// Version 3 (ADR 0216) moves datagrams through shared rings, so a send or a
// receive costs no channel message while both sides keep busy. The channel
// keeps open, bind, connect, close, Ready, WaitReady and Option, and
// carries OpDoorbell notifications both ways. Send, SendTo, RecvFrom and
// Release are not version 3 opcodes.
//
// Each pool ends in one control page. The client writes the page after its
// TX slots and netd only reads it; netd writes the page after its RX slots
// and the client only reads it. Every shared word has one writer, and the
// pool rights PID 1 grants already enforce it.
//
// Client page, at TXSlots3*SlotSize in the TX pool:
//
//	0   client armed counter (the client consumes Done and RX)
//	4   client rung counter (the client answers netd's arm)
//	8   Done head
//	12  RX head
//	16  TX tail
//	20  Release tail
//	64  TX ring, TXEntries3 entries
//	    Release ring, ReleaseEntries3 entries
//
// Service page, at RXSlots3*SlotSize in the RX pool:
//
//	0   netd armed counter (netd consumes TX and Release)
//	4   netd rung counter (netd answers the client's arm)
//	8   TX head
//	12  Release head
//	16  Done tail
//	20  RX tail
//	24  Magic3, stored when the session starts
//	64  Done ring, DoneEntries3 entries
//	    RX ring, RXEntries3 entries
//
// The client produces on TX (EntrySend) and Release (EntryRelease,
// EntryCredit). netd produces on Done (EntrySent) and RX (EntryDatagram). A
// producer that publishes calls the doorbell rule of shmring and sends
// OpDoorbell when the consumer armed.
//
// Slots. A TX slot belongs to the client until EntrySend names it, then to
// netd until EntrySent hands it back with the send's result. An RX slot
// belongs to netd until EntryDatagram names it, then to the client until
// EntryRelease. The side handing a slot over numbers its hand-offs of that
// slot 1, 2, 3, and the side handing it back repeats the number. netd ends
// the session when a TX entry names a slot it holds or a generation out of
// turn, or a Release names a slot the client does not hold or a stale
// generation.
//
// Receives. EntryCredit adds Count receives to an endpoint; an endpoint
// holds at most MaxCredit3. While credit remains and a datagram waits, netd
// fills a free RX slot with it and publishes EntryDatagram with the
// endpoint's readiness after it, at the cost of one credit. OpClose drops
// the endpoint's credit. netd takes every TX entry the client published
// before it handles OpClose, so a send queued ahead of the close goes out
// first; an entry for an endpoint already closed comes back with
// ResultInvalidArgument.

import (
	"encoding/binary"
	"sync/atomic"

	"goos/kernel/ipc"
	"goos/userspace/shmring"
)

const (
	Version3 = uint8(3)
	TXSlots3 = 16
	RXSlots3 = 16
	// MaxCredit3 bounds the receives one endpoint has posted.
	MaxCredit3 = RXSlots3

	TXEntries3      = 32
	ReleaseEntries3 = 64
	DoneEntries3    = 32
	RXEntries3      = 32
	EntrySize3      = 32

	// TXPoolPages3 and RXPoolPages3 size a version 3 session's pools: the
	// slots and one control page each.
	TXPoolPages3 = TXSlots3*SlotSize/pageSize + 1
	RXPoolPages3 = RXSlots3*SlotSize/pageSize + 1
	// TXControl3 and RXControl3 are where each pool's control page starts.
	TXControl3 = TXSlots3 * SlotSize
	RXControl3 = RXSlots3 * SlotSize

	// Magic3 marks a service page netd initialized.
	Magic3 = uint32(0x33504455)

	offArmed      = 0
	offRung       = 4
	offFirstHead  = 8
	offSecondHead = 12
	offFirstTail  = 16
	offSecondTail = 20
	offMagic      = 24
	firstRingBase = 64
	clientRelease = firstRingBase + TXEntries3*EntrySize3
	serviceRX     = firstRingBase + DoneEntries3*EntrySize3
	clientEnd     = clientRelease + ReleaseEntries3*EntrySize3
	serviceEnd    = serviceRX + RXEntries3*EntrySize3
)

// Each ring holds every slot that can be on it at once.
var (
	_ [pageSize - clientEnd]byte
	_ [pageSize - serviceEnd]byte
	_ [TXEntries3 - TXSlots3]byte
	_ [DoneEntries3 - TXSlots3]byte
	_ [RXEntries3 - RXSlots3]byte
	_ [ReleaseEntries3 - RXSlots3]byte
)

// OpDoorbell is a version 3 notification either side sends when the other
// armed its doorbell and a ring of it gained entries. Its request names
// nothing; netd's notification is the reply body with only the opcode set.
const OpDoorbell = lastOpcode + 1

// Kind is a ring entry's kind.
type Kind uint8

const (
	// EntrySend, on TX: send Length bytes of Slot from Endpoint, to Address
	// and Port with FlagTo, else to the connected peer.
	EntrySend Kind = iota + 1
	// EntryRelease, on Release: RX Slot goes back to netd.
	EntryRelease
	// EntryCredit, on Release: Endpoint may take Value more receives.
	EntryCredit
	// EntrySent, on Done: TX Slot comes back with the send's Result.
	EntrySent
	// EntryDatagram, on RX: Slot holds Length bytes for Endpoint from
	// Address and Port, sent to Local; Mask and Value are the endpoint's
	// readiness after it.
	EntryDatagram
	lastKind = EntryDatagram
)

// FlagTo marks an EntrySend that names its destination.
const FlagTo uint8 = 1

// Entry is one decoded ring entry. Which fields a kind uses is fixed; the
// rest are zero.
type Entry struct {
	Kind       Kind
	Slot       uint8
	Flags      uint8
	Result     Result
	Endpoint   uint32
	Address    [addressSize]byte
	Port       uint16
	Length     uint16
	Generation uint32
	Local      [addressSize]byte
	Mask       uint16
	Value      uint32
}

const (
	entryKind       = 0
	entrySlot       = 1
	entryFlags      = 2
	entryResult     = 3
	entryEndpoint   = 4
	entryAddress    = 8
	entryPort       = 12
	entryLength     = 14
	entryGeneration = 16
	entryLocal      = 20
	entryMask       = 24
	entryReserved   = 26
	entryValue      = 28
)

func validEntry(e *Entry) bool {
	if e.Kind == 0 || e.Kind > lastKind || e.Result > lastResult {
		return false
	}
	addressed := e.Address != noAddress || e.Port != 0
	plainSlot := e.Endpoint == 0 && e.Flags == 0 && e.Result == ResultOK && !addressed && e.Length == 0 &&
		e.Local == noAddress && e.Mask == 0 && e.Value == 0
	switch e.Kind {
	case EntrySend:
		if e.Flags&^FlagTo != 0 || (e.Flags&FlagTo != 0) != (e.Address != noAddress && e.Port != 0) ||
			e.Flags&FlagTo == 0 && addressed {
			return false
		}
		return e.Endpoint != 0 && e.Slot < TXSlots3 && e.Length <= MaxPayload && e.Generation != 0 && e.Result == ResultOK &&
			e.Local == noAddress && e.Mask == 0 && e.Value == 0
	case EntryRelease:
		return plainSlot && e.Slot < RXSlots3 && e.Generation != 0
	case EntryCredit:
		return e.Endpoint != 0 && e.Slot == 0 && e.Flags == 0 && e.Result == ResultOK && !addressed && e.Length == 0 &&
			e.Generation == 0 && e.Local == noAddress && e.Mask == 0 && e.Value != 0 && e.Value <= MaxCredit3
	case EntrySent:
		return e.Endpoint != 0 && e.Slot < TXSlots3 && e.Generation != 0 && e.Flags == 0 && !addressed && e.Length == 0 &&
			e.Local == noAddress && e.Mask == 0 && e.Value == 0
	case EntryDatagram:
		return e.Endpoint != 0 && e.Slot < RXSlots3 && e.Generation != 0 && e.Flags == 0 && e.Result == ResultOK &&
			e.Address != noAddress && e.Length <= MaxPayload && e.Mask&^ReadyAll == 0
	}
	return false
}

// EncodeEntry writes e into p, a ring entry Reserve handed out. It fails
// for an entry the protocol does not allow.
func EncodeEntry(e *Entry, p []byte) bool {
	if len(p) < EntrySize3 || !validEntry(e) {
		return false
	}
	p[entryKind], p[entrySlot], p[entryFlags], p[entryResult] = byte(e.Kind), e.Slot, e.Flags, byte(e.Result)
	binary.LittleEndian.PutUint32(p[entryEndpoint:], e.Endpoint)
	copy(p[entryAddress:], e.Address[:])
	binary.LittleEndian.PutUint16(p[entryPort:], e.Port)
	binary.LittleEndian.PutUint16(p[entryLength:], e.Length)
	binary.LittleEndian.PutUint32(p[entryGeneration:], e.Generation)
	copy(p[entryLocal:], e.Local[:])
	binary.LittleEndian.PutUint16(p[entryMask:], e.Mask)
	binary.LittleEndian.PutUint32(p[entryValue:], e.Value)
	return true
}

// DecodeEntry reads one entry and checks it against the protocol.
func DecodeEntry(p []byte, e *Entry) bool {
	if len(p) < EntrySize3 || p[entryReserved] != 0 || p[entryReserved+1] != 0 {
		return false
	}
	*e = Entry{Kind: Kind(p[entryKind]), Slot: p[entrySlot], Flags: p[entryFlags], Result: Result(p[entryResult]),
		Endpoint:   binary.LittleEndian.Uint32(p[entryEndpoint:]),
		Port:       binary.LittleEndian.Uint16(p[entryPort:]),
		Length:     binary.LittleEndian.Uint16(p[entryLength:]),
		Generation: binary.LittleEndian.Uint32(p[entryGeneration:]),
		Mask:       binary.LittleEndian.Uint16(p[entryMask:]),
		Value:      binary.LittleEndian.Uint32(p[entryValue:])}
	copy(e.Address[:], p[entryAddress:])
	copy(e.Local[:], p[entryLocal:])
	return validEntry(e)
}

// allowedOn reports whether kind may travel on the ring a side consumes.
func allowedOn(kind Kind, ring int) bool {
	switch ring {
	case RingTX:
		return kind == EntrySend
	case RingRelease:
		return kind == EntryRelease || kind == EntryCredit
	case RingDone:
		return kind == EntrySent
	case RingRX:
		return kind == EntryDatagram
	}
	return false
}

// The four rings of a session.
const (
	RingTX = iota
	RingRelease
	RingDone
	RingRX
)

// Rings3 is one side's view of a session's rings. The client's view
// produces on TX and Release and consumes Done and RX; netd's view the
// other way round.
type Rings3 struct {
	rings [4]shmring.Ring
	// own is the control page this side writes, peer the page it reads.
	own, peer []byte
	service   bool
}

// ClientRings3 builds the client's view from the two control pages: the
// one at TXControl3 in the TX pool and the one at RXControl3 in the RX pool.
func ClientRings3(client, service []byte) (Rings3, bool) {
	return newRings3(client, service, false)
}

// ServiceRings3 builds netd's view of the same two pages.
func ServiceRings3(client, service []byte) (Rings3, bool) {
	return newRings3(client, service, true)
}

func newRings3(client, service []byte, isService bool) (Rings3, bool) {
	if len(client) < pageSize || len(service) < pageSize {
		return Rings3{}, false
	}
	var r Rings3
	word := shmring.Word
	r.rings[RingTX] = shmring.New(word(service, offFirstHead), word(client, offFirstTail), client[firstRingBase:], TXEntries3, EntrySize3)
	r.rings[RingRelease] = shmring.New(word(service, offSecondHead), word(client, offSecondTail), client[clientRelease:], ReleaseEntries3, EntrySize3)
	r.rings[RingDone] = shmring.New(word(client, offFirstHead), word(service, offFirstTail), service[firstRingBase:], DoneEntries3, EntrySize3)
	r.rings[RingRX] = shmring.New(word(client, offSecondHead), word(service, offSecondTail), service[serviceRX:], RXEntries3, EntrySize3)
	r.own, r.peer, r.service = client, service, isService
	if isService {
		r.own, r.peer = service, client
	}
	return r, true
}

// InitService clears netd's control page for a new session and marks it.
func InitService(service []byte) {
	clear(service[:pageSize])
	atomic.StoreUint32(shmring.Word(service, offMagic), Magic3)
}

// ServiceReady reports whether netd initialized its control page.
func ServiceReady(service []byte) bool {
	return len(service) >= pageSize && atomic.LoadUint32(shmring.Word(service, offMagic)) == Magic3
}

func (r *Rings3) produces(ring int) bool {
	return r.service == (ring == RingDone || ring == RingRX)
}

// Push publishes e on ring, which this side produces. ok false with
// shmring.Full means no room now; any other status is a broken ring or an
// entry the protocol does not allow.
func (r *Rings3) Push(ring int, e *Entry) shmring.Status {
	if ring < RingTX || ring > RingRX || !r.produces(ring) || !allowedOn(e.Kind, ring) {
		return shmring.Broken
	}
	p, status := r.rings[ring].Reserve()
	if status != shmring.OK {
		return status
	}
	if !EncodeEntry(e, p) {
		return shmring.Broken
	}
	r.rings[ring].Publish()
	return shmring.OK
}

// Pop takes the oldest entry of ring, which this side consumes. Empty means
// none; Broken a corrupt ring or an entry that does not belong on it.
func (r *Rings3) Pop(ring int, e *Entry) shmring.Status {
	if ring < RingTX || ring > RingRX || r.produces(ring) {
		return shmring.Broken
	}
	p, status := r.rings[ring].Front()
	if status != shmring.OK {
		return status
	}
	if !DecodeEntry(p, e) || !allowedOn(e.Kind, ring) {
		return shmring.Broken
	}
	r.rings[ring].Consume()
	return shmring.OK
}

// Room reports how many entries ring takes now.
func (r *Rings3) Room(ring int) uint32 { return r.rings[ring].Room() }

// Pending reports whether a ring this side consumes holds entries.
func (r *Rings3) Pending() bool {
	if r.service {
		return r.rings[RingTX].Pending() || r.rings[RingRelease].Pending()
	}
	return r.rings[RingDone].Pending() || r.rings[RingRX].Pending()
}

// Arm records that this side is about to sleep; the caller then checks its
// rings once more.
func (r *Rings3) Arm() { shmring.Arm(shmring.Word(r.own, offArmed)) }

// ShouldRing reports whether the peer armed since this side last rang it,
// and marks the arm answered. A producer calls it after it publishes.
func (r *Rings3) ShouldRing() bool {
	return shmring.ShouldRing(shmring.Word(r.peer, offArmed), shmring.Word(r.own, offRung))
}

// EncodeDoorbell builds the client's OpDoorbell notification.
func EncodeDoorbell(out *ipc.Message) {
	prepare(out, ipc.KindNotification, 0, RequestSize)
	out.Payload[requestOpcode] = byte(OpDoorbell)
}

// EncodeServiceDoorbell builds netd's OpDoorbell notification.
func EncodeServiceDoorbell(out *ipc.Message) {
	prepare(out, ipc.KindNotification, 0, ReplySize)
	out.Payload[replyOpcode] = byte(OpDoorbell)
}

// IsDoorbell reports whether in is a well-formed OpDoorbell notification
// from either side.
func IsDoorbell(in *ipc.Message) bool {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindNotification || in.Correlation != 0 ||
		in.HandleCount != 0 || (in.PayloadSize != RequestSize && in.PayloadSize != ReplySize) {
		return false
	}
	return in.Payload[0] == byte(OpDoorbell) && zero(in.Payload[1:in.PayloadSize])
}

// Version3Request reports whether a decoded request is one a version 3
// session serves over its channel.
func Version3Request(r *Request) bool {
	switch r.Opcode {
	case OpSend, OpSendTo, OpRecvFrom, OpRelease:
		return false
	}
	return r.Release == 0
}
