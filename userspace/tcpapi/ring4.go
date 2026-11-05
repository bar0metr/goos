package tcpapi

// Version 4 (Layout4) moves stream data through shared rings, so a 16 KiB
// slot costs no channel message. The channel keeps open, bind, listen,
// connect, accept, options, shutdown, close, readiness waits and errors, and
// carries OpDoorbell notifications both ways. OpWrite, OpRead and OpRelease
// are not version 4 opcodes.
//
// Each pool ends in a control page. The client writes the page of the TX
// pool and netd only reads it; netd writes the page of the RX pool and the
// client only reads it. Every shared word has one writer, and the pool
// rights PID 1 grants already enforce it.
//
// Client page, at TXSlots4*SlotSize4 in the TX pool:
//
//	0   client armed counter (the client consumes TXDone and RX)
//	4   client rung counter (the client answers netd's arm)
//	8   TXDone head
//	12  RX head
//	16  TX tail
//	20  Release tail
//	64  TX ring, TXEntries4 entries
//	    Release ring, ReleaseEntries4 entries
//
// Service page, at RXSlots4*SlotSize4 in the RX pool:
//
//	0   netd armed counter (netd consumes TX and Release)
//	4   netd rung counter (netd answers the client's arm)
//	8   TX head
//	12  Release head
//	16  TXDone tail
//	20  RX tail
//	24  Magic4, stored when the session starts
//	64  TXDone ring, TXDoneEntries4 entries
//	    RX ring, RXEntries4 entries
//	    readiness, Endpoints4 entries of readinessSize bytes
//
// The client produces on TX (EntryWrite) and Release (EntryRelease,
// EntryCredit, EntryForget). netd produces on TXDone (EntryWriteDone,
// EntryClosed) and RX (EntryData, EntryEnd, EntryAccepted). A producer that
// publishes calls the doorbell rule of shmring and sends OpDoorbell when the
// consumer armed.
//
// Slots. A TX slot belongs to the client until it sends EntryWrite, then to
// netd until EntryWriteDone names it. An RX slot belongs to netd until
// EntryData names it, then to the client until EntryRelease. Each slot has a
// generation: the client numbers its Writes of a slot 1, 2, 3 and so on,
// netd numbers its fills of an RX slot the same way, and the side handing a
// slot back repeats the number. netd ends the session when a TX entry names
// a slot it holds or a generation out of turn, or a Release names a slot the
// client does not hold or a stale generation.
//
// Writes. EntryWrite names the endpoint, its table index, the slot, the
// bytes in it and the stream offset of its first byte: the count of bytes
// the client wrote to the endpoint before, mod 2^32, as in version 3. netd
// queues the slot behind the endpoint's earlier ones and hands it back once
// the stack took every byte, or with the error that stopped it. A Write
// whose offset is not the end of the queue comes back with
// ResultInvalidArgument and nothing taken.
//
// Reads. EntryCredit posts Length fills for an endpoint; an endpoint holds
// at most MaxCredit4. While credit remains netd fills a free RX slot with
// whatever bytes arrived and publishes EntryData, or EntryEnd with the
// end-of-stream or the error that ends the stream. Each costs one credit.
//
// Lifecycle. OpOpenConnect opens an endpoint and connects it; a failure
// that leaves nothing open carries endpoint 0. OpAcceptAhead lets netd accept
// up to Backlog children of a listener without further requests; each
// arrives as EntryAccepted. OpClose answers at once and hands the endpoint
// to netd: netd sends what the client queued, hands those TX slots back,
// closes the connection and publishes EntryClosed. The client keeps the RX
// slots it holds until it releases them, and sends EntryForget for the
// closed endpoint once it no longer refers to it; netd reuses the table
// entry only then, so a stale entry cannot reach a new endpoint.
//
// The replies to OpOpen, OpOpenConnect and OpAccept name the endpoint's
// table index in Slot; it selects the endpoint's readiness entry and goes in
// every ring entry about the endpoint.

import (
	"encoding/binary"
	"sync/atomic"

	"goos/kernel/ipc"
	"goos/userspace/shmring"
)

const (
	Version4     = uint16(4)
	RequestSize4 = RequestSize3
	SlotSize4    = 16384
	TXSlots4     = 16
	RXSlots4     = 16
	// Endpoints4 is the endpoint table of a version 4 session.
	Endpoints4 = 64
	// MaxCredit4 bounds the fills one endpoint has posted.
	MaxCredit4 = RXSlots4

	TXEntries4      = 32
	ReleaseEntries4 = 64
	TXDoneEntries4  = 32
	RXEntries4      = 32
	EntrySize4      = 32

	// Magic4 marks a service page netd initialized.
	Magic4 = uint32(0x34505447)

	offArmed       = 0
	offRung        = 4
	offFirstHead   = 8
	offSecondHead  = 12
	offFirstTail   = 16
	offSecondTail  = 20
	offMagic       = 24
	firstRingBase  = 64
	clientRelease  = firstRingBase + TXEntries4*EntrySize4
	serviceRX      = firstRingBase + TXDoneEntries4*EntrySize4
	readinessBase  = serviceRX + RXEntries4*EntrySize4
	readinessSize  = 24
	clientPageEnd  = clientRelease + ReleaseEntries4*EntrySize4
	servicePageEnd = readinessBase + Endpoints4*readinessSize
)

var _ [pageSize - clientPageEnd]byte
var _ [pageSize - servicePageEnd]byte

// The version 4 opcodes past version 3's.
const (
	// OpOpenConnect opens an endpoint and connects it to Address and Port.
	OpOpenConnect = lastOpcode + 1 + iota
	// OpAcceptAhead sets how many children of the listener netd accepts
	// before the client asks: Backlog, zero to stop.
	OpAcceptAhead
	// OpDoorbell is a notification either side sends when the other armed
	// its doorbell and a ring of it gained entries.
	OpDoorbell
	lastOpcode4 = OpDoorbell
)

// Layout4 is version 4: sixteen TX and sixteen RX slots of 16 KiB, each
// pool followed by its control page.
var Layout4 = Layout{Version: Version4, RequestSize: RequestSize4, SlotSize: SlotSize4, TXSlots: TXSlots4, RXSlots: RXSlots4}

func (l Layout) controlBytes() uint64 {
	if l.Version == Version4 {
		return pageSize
	}
	return 0
}

func notification(opcode Opcode) bool { return opcode == OpRelease || opcode == OpDoorbell }

func validRequest4(r *Request) bool {
	if r.Release != 0 || r.Offset != 0 {
		return false
	}
	none := r.Address == noAddress && r.Port == 0
	plain := r.Slot == 0 && r.Length == 0 && r.Mask == 0 && r.Option == 0
	switch r.Opcode {
	case OpWrite, OpRead, OpRelease:
		return false
	case OpOpenConnect:
		return plain && r.Endpoint == 0 && r.Address != noAddress && r.Port != 0 && r.Backlog == 0 && r.Flags&^FlagNoWait == 0
	case OpAcceptAhead:
		return plain && r.Endpoint != 0 && none && r.Flags == 0 && r.Backlog <= Endpoints4
	case OpDoorbell:
		return plain && r.Endpoint == 0 && none && r.Flags == 0 && r.Backlog == 0
	}
	return validRequest(&Layout3, r)
}

// validReply4 checks a version 4 reply. Slot carries the table index of an
// endpoint that Open, OpenConnect or Accept made.
func validReply4(r *Reply) bool {
	if r.Slot >= Endpoints4 || r.Result > lastResult {
		return false
	}
	indexed := r.Result == ResultOK || r.Opcode == OpOpenConnect && r.Result == ResultInProgress
	switch r.Opcode {
	case OpWrite, OpRead, OpRelease, OpDoorbell:
		return false
	case OpOpen, OpAccept:
	case OpOpenConnect:
		readiness := r.Mask != 0 || r.Value != 0 || r.Room != 0 || r.Error != ResultOK || r.Length != 0
		if !indexed {
			return !readiness && r.Endpoint == 0 && r.Slot == 0 && r.Address == noAddress && r.Port == 0 && r.Local == noAddress && r.LocalPort == 0
		}
		return !readiness && r.Endpoint != 0 && r.Address != noAddress && r.Port != 0 && r.Local != noAddress && r.LocalPort != 0
	case OpAcceptAhead:
		return r.Endpoint != 0 && r.Slot == 0 && r.Error == ResultOK && r.Length == 0 && r.Address == noAddress && r.Port == 0 &&
			r.Local == noAddress && r.LocalPort == 0 && r.Mask == 0 && r.Value == 0 && r.Room == 0 && r.Result != ResultWouldBlock &&
			r.Result != ResultInProgress && r.Result != ResultEOF
	default:
		if r.Slot != 0 {
			return false
		}
	}
	if r.Slot != 0 && !indexed {
		return false
	}
	plain := *r
	plain.Slot = 0
	return validReply(&Layout3, &plain)
}

// EncodeDoorbell4 builds the OpDoorbell notification either side sends.
func EncodeDoorbell4(out *ipc.Message) bool {
	return Layout4.EncodeRequest(&doorbell, 0, out)
}

// doorbell is the one OpDoorbell request; the encoder only reads it.
var doorbell = Request{Opcode: OpDoorbell}

// IsDoorbell4 reports whether in is a well-formed version 4 doorbell.
func IsDoorbell4(in *ipc.Message) bool {
	var r Request
	_, ok := Layout4.DecodeRequest(in, &r)
	return ok && r.Opcode == OpDoorbell
}

// EntryKind names a version 4 ring entry.
type EntryKind uint8

const (
	// EntryWrite (TX ring): Length bytes of Slot for Endpoint, the first at
	// stream offset Offset.
	EntryWrite EntryKind = iota + 1
	// EntryRelease (Release ring): the client hands RX Slot back.
	EntryRelease
	// EntryCredit (Release ring): Length more fills for Endpoint.
	EntryCredit
	// EntryForget (Release ring): the client gives up a closed Endpoint.
	EntryForget
	// EntryWriteDone (TXDone ring): TX Slot back; Length bytes taken, and
	// Result.
	EntryWriteDone
	// EntryClosed (TXDone ring): a closed endpoint is gone from the stack.
	EntryClosed
	// EntryData (RX ring): Length stream bytes in RX Slot.
	EntryData
	// EntryEnd (RX ring): the stream ended with Result, ResultEOF for the
	// peer's FIN.
	EntryEnd
	// EntryAccepted (RX ring): a child accepted ahead. Index and Endpoint
	// name the child, Slot the listener's index and Generation the
	// listener's endpoint; the addresses are the child's tuple.
	EntryAccepted
	lastEntry = EntryAccepted
)

// Entry is one decoded version 4 ring entry.
//
//	0   kind
//	1   slot
//	2   index: the endpoint's table index
//	3   result
//	4   length
//	8   endpoint
//	12  generation
//	16  offset
//	20  address, 24 port, 26 local port, 28 local address
type Entry struct {
	Kind       EntryKind
	Slot       uint8
	Index      uint8
	Result     Result
	Length     uint32
	Endpoint   uint32
	Generation uint32
	Offset     uint32
	Address    [4]byte
	Port       uint16
	LocalPort  uint16
	Local      [4]byte
}

// ValidEntry checks an entry's fields against its kind.
func ValidEntry(e *Entry) bool {
	addressed := e.Address != noAddress || e.Port != 0 || e.Local != noAddress || e.LocalPort != 0
	named := e.Index < Endpoints4 && e.Endpoint != 0
	if e.Kind != EntryAccepted && addressed || e.Result > lastResult {
		return false
	}
	switch e.Kind {
	case EntryWrite:
		return named && e.Slot < TXSlots4 && e.Length <= SlotSize4 && e.Result == ResultOK
	case EntryRelease:
		return e.Slot < RXSlots4 && e.Index == 0 && e.Endpoint == 0 && e.Length == 0 && e.Offset == 0 && e.Result == ResultOK
	case EntryCredit:
		return named && e.Slot == 0 && e.Length != 0 && e.Length <= MaxCredit4 && e.Generation == 0 && e.Offset == 0 && e.Result == ResultOK
	case EntryForget, EntryClosed:
		return named && e.Slot == 0 && e.Length == 0 && e.Generation == 0 && e.Offset == 0 && e.Result == ResultOK
	case EntryWriteDone:
		return named && e.Slot < TXSlots4 && e.Length <= SlotSize4 && e.Offset == 0
	case EntryData:
		return named && e.Slot < RXSlots4 && e.Length != 0 && e.Length <= SlotSize4 && e.Offset == 0 && e.Result == ResultOK
	case EntryEnd:
		return named && e.Slot == 0 && e.Length == 0 && e.Generation == 0 && e.Offset == 0 && e.Result != ResultOK
	case EntryAccepted:
		return named && e.Slot < Endpoints4 && e.Generation != 0 && e.Length == 0 && e.Offset == 0 && e.Result == ResultOK &&
			e.Address != noAddress && e.Port != 0 && e.Local != noAddress && e.LocalPort != 0
	}
	return false
}

func putEntry(p []byte, e *Entry) {
	p[0], p[1], p[2], p[3] = byte(e.Kind), e.Slot, e.Index, byte(e.Result)
	binary.LittleEndian.PutUint32(p[4:], e.Length)
	binary.LittleEndian.PutUint32(p[8:], e.Endpoint)
	binary.LittleEndian.PutUint32(p[12:], e.Generation)
	binary.LittleEndian.PutUint32(p[16:], e.Offset)
	copy(p[20:24], e.Address[:])
	binary.LittleEndian.PutUint16(p[24:], e.Port)
	binary.LittleEndian.PutUint16(p[26:], e.LocalPort)
	copy(p[28:32], e.Local[:])
}

func getEntry(p []byte) Entry {
	e := Entry{Kind: EntryKind(p[0]), Slot: p[1], Index: p[2], Result: Result(p[3]), Length: binary.LittleEndian.Uint32(p[4:]),
		Endpoint: binary.LittleEndian.Uint32(p[8:]), Generation: binary.LittleEndian.Uint32(p[12:]),
		Offset: binary.LittleEndian.Uint32(p[16:]), Port: binary.LittleEndian.Uint16(p[24:]), LocalPort: binary.LittleEndian.Uint16(p[26:])}
	copy(e.Address[:], p[20:24])
	copy(e.Local[:], p[28:32])
	return e
}

// Control is a session's two control pages. Client methods write only the
// client page and Service methods only the service page.
type Control struct {
	client, service []byte
}

// ControlOf finds the control pages at the end of a version 4 session's
// pools.
func ControlOf(tx, rx []byte) (Control, bool) {
	txAt, rxAt := Layout4.Offset(TXSlots4), Layout4.Offset(RXSlots4)
	if uint64(len(tx)) < txAt+pageSize || uint64(len(rx)) < rxAt+pageSize {
		return Control{}, false
	}
	return Control{client: tx[txAt : txAt+pageSize], service: rx[rxAt : rxAt+pageSize]}, true
}

// InitService clears the service page and marks it, for a new session.
func (c *Control) InitService() {
	for offset := 0; offset < servicePageEnd; offset += 4 {
		atomic.StoreUint32(shmring.Word(c.service, offset), 0)
	}
	atomic.StoreUint32(shmring.Word(c.service, offMagic), Magic4)
}

// ServiceReady reports whether netd initialized the service page.
func (c *Control) ServiceReady() bool {
	return atomic.LoadUint32(shmring.Word(c.service, offMagic)) == Magic4
}

func (c *Control) tx() shmring.Ring {
	return shmring.New(shmring.Word(c.service, offFirstHead), shmring.Word(c.client, offFirstTail), c.client[firstRingBase:], TXEntries4, EntrySize4)
}

func (c *Control) release() shmring.Ring {
	return shmring.New(shmring.Word(c.service, offSecondHead), shmring.Word(c.client, offSecondTail), c.client[clientRelease:], ReleaseEntries4, EntrySize4)
}

func (c *Control) txDone() shmring.Ring {
	return shmring.New(shmring.Word(c.client, offFirstHead), shmring.Word(c.service, offFirstTail), c.service[firstRingBase:], TXDoneEntries4, EntrySize4)
}

func (c *Control) rx() shmring.Ring {
	return shmring.New(shmring.Word(c.client, offSecondHead), shmring.Word(c.service, offSecondTail), c.service[serviceRX:], RXEntries4, EntrySize4)
}

// kinds is a set of EntryKind bits.
func kinds(list ...EntryKind) uint16 {
	set := uint16(0)
	for _, kind := range list {
		set |= 1 << kind
	}
	return set
}

var (
	txKinds      = kinds(EntryWrite)
	releaseKinds = kinds(EntryRelease, EntryCredit, EntryForget)
	txDoneKinds  = kinds(EntryWriteDone, EntryClosed)
	rxKinds      = kinds(EntryData, EntryEnd, EntryAccepted)
)

func push(r shmring.Ring, e *Entry, allowed uint16) shmring.Status {
	if !ValidEntry(e) || allowed&(1<<e.Kind) == 0 {
		return shmring.Broken
	}
	p, status := r.Reserve()
	if status != shmring.OK {
		return status
	}
	putEntry(p, e)
	r.Publish()
	return shmring.OK
}

func pop(r shmring.Ring, out *Entry, allowed uint16) shmring.Status {
	p, status := r.Front()
	if status != shmring.OK {
		return status
	}
	e := getEntry(p)
	if !ValidEntry(&e) || allowed&(1<<e.Kind) == 0 {
		return shmring.Broken
	}
	r.Consume()
	*out = e
	return shmring.OK
}

// The client's side of the rings.

func (c *Control) PushTX(e *Entry) shmring.Status      { return push(c.tx(), e, txKinds) }
func (c *Control) PushRelease(e *Entry) shmring.Status { return push(c.release(), e, releaseKinds) }
func (c *Control) PopTXDone(out *Entry) shmring.Status { return pop(c.txDone(), out, txDoneKinds) }
func (c *Control) PopRX(out *Entry) shmring.Status     { return pop(c.rx(), out, rxKinds) }

// ReleaseRoom reports how many entries the client can add to Release now.
func (c *Control) ReleaseRoom() uint32 { r := c.release(); return r.Room() }

// ClientPending reports whether TXDone or RX hold entries for the client.
func (c *Control) ClientPending() bool {
	done, rx := c.txDone(), c.rx()
	return done.Pending() || rx.Pending()
}

// ClientArm arms the client's doorbell before it sleeps; the client then
// checks ClientPending once more.
func (c *Control) ClientArm() { shmring.Arm(shmring.Word(c.client, offArmed)) }

// ClientShouldRing reports, after the client published, whether netd armed
// and needs an OpDoorbell.
func (c *Control) ClientShouldRing() bool {
	return shmring.ShouldRing(shmring.Word(c.service, offArmed), shmring.Word(c.client, offRung))
}

// netd's side of the rings.

func (c *Control) PopTX(out *Entry) shmring.Status      { return pop(c.tx(), out, txKinds) }
func (c *Control) PopRelease(out *Entry) shmring.Status { return pop(c.release(), out, releaseKinds) }
func (c *Control) PushTXDone(e *Entry) shmring.Status   { return push(c.txDone(), e, txDoneKinds) }
func (c *Control) PushRX(e *Entry) shmring.Status       { return push(c.rx(), e, rxKinds) }

// TXDoneRoom and RXRoom report how many entries netd can add now; zero
// when the client broke the indices.
func (c *Control) TXDoneRoom() uint32 { r := c.txDone(); return r.Room() }
func (c *Control) RXRoom() uint32     { r := c.rx(); return r.Room() }

// ServicePending reports whether TX or Release hold entries for netd.
func (c *Control) ServicePending() bool {
	tx, release := c.tx(), c.release()
	return tx.Pending() || release.Pending()
}

// ServiceArm arms netd's doorbell before it sleeps on the session.
func (c *Control) ServiceArm() { shmring.Arm(shmring.Word(c.service, offArmed)) }

// ServiceShouldRing reports, after netd published, whether the client armed
// and needs an OpDoorbell.
func (c *Control) ServiceShouldRing() bool {
	return shmring.ShouldRing(shmring.Word(c.client, offArmed), shmring.Word(c.service, offRung))
}

// Readiness is one endpoint's entry of the readiness page: the stack's mask,
// readable bytes and send room, and the fills the endpoint has posted.
// Endpoint is zero for an unused entry.
type Readiness struct {
	Endpoint uint32
	Mask     uint16
	Readable uint32
	Room     uint32
	Credit   uint32
}

// StoreReadiness writes entry index under its sequence word: odd while netd
// writes it.
func (c *Control) StoreReadiness(index int, r *Readiness) {
	if index < 0 || index >= Endpoints4 {
		return
	}
	base := readinessBase + index*readinessSize
	seq := shmring.Word(c.service, base)
	atomic.StoreUint32(seq, atomic.LoadUint32(seq)+1)
	atomic.StoreUint32(shmring.Word(c.service, base+4), r.Endpoint)
	atomic.StoreUint32(shmring.Word(c.service, base+8), uint32(r.Mask))
	atomic.StoreUint32(shmring.Word(c.service, base+12), r.Readable)
	atomic.StoreUint32(shmring.Word(c.service, base+16), r.Room)
	atomic.StoreUint32(shmring.Word(c.service, base+20), r.Credit)
	atomic.StoreUint32(seq, atomic.LoadUint32(seq)+1)
}

// LoadReadiness reads entry index. It reports false when netd kept
// rewriting the entry through every attempt.
func (c *Control) LoadReadiness(index int) (Readiness, bool) {
	if index < 0 || index >= Endpoints4 {
		return Readiness{}, false
	}
	base := readinessBase + index*readinessSize
	seq := shmring.Word(c.service, base)
	for range 16 {
		before := atomic.LoadUint32(seq)
		if before&1 != 0 {
			continue
		}
		r := Readiness{Endpoint: atomic.LoadUint32(shmring.Word(c.service, base+4)),
			Mask:     uint16(atomic.LoadUint32(shmring.Word(c.service, base+8))),
			Readable: atomic.LoadUint32(shmring.Word(c.service, base+12)),
			Room:     atomic.LoadUint32(shmring.Word(c.service, base+16)),
			Credit:   atomic.LoadUint32(shmring.Word(c.service, base+20))}
		if atomic.LoadUint32(seq) == before {
			return r, true
		}
	}
	return Readiness{}, false
}
