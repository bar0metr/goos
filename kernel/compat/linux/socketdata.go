package linux

import "goos/userspace/linuxsockapi"

// The kernel's half of a family's data plane (ADR 0210): which TX and RX
// slots of the pool the kernel owns, and the RX slots each socket has
// queued. Each step runs under the tables lock and works the ring page the
// caller passes; the caller copies payload between steps, in slots only it
// owns at that point.
//
// A TX slot is free, being filled by one call, or committed to the
// service. A call reserves one, copies into it and commits it on the submit
// ring; the service hands it back on the complete ring with the generation
// the commit gave it. An RX slot the service publishes joins its socket's
// queue in ring order; reads take bytes from the oldest slot first and
// hand a slot back on the submit ring once they consumed all of it.

type txState uint8

const (
	txFree txState = iota
	txFilling
	txCommitted
)

type txSlot struct {
	state      txState
	generation uint32
	socket     uint64
}

type rxSlot struct {
	held       bool
	generation uint32
	socket     uint64
	length     uint32
	offset     uint32
	order      uint32
	address    [4]byte
	port       uint16
	// orphan: the socket was unknown when the slot came; ReleaseOrphans
	// hands it back unless the socket turned up.
	orphan bool
}

type dataPlane struct {
	tx      [linuxsockapi.TXSlots]txSlot
	rx      [linuxsockapi.RXSlots]rxSlot
	txGen   uint32
	rxOrder uint32
	// accepted holds the identities of accepted children whose reply came
	// and whose socket the accepting call has not installed yet. Their RX
	// slots stay queued: the service reads ahead from the moment it
	// answered the Accept.
	accepted [MaxSocketRequests]uint64
}

func (data *dataPlane) noteAccepted(identity uint64) {
	for index := range data.accepted {
		if data.accepted[index] == 0 || data.accepted[index] == identity {
			data.accepted[index] = identity
			return
		}
	}
}

func (data *dataPlane) forgetAccepted(identity uint64) {
	for index := range data.accepted {
		if data.accepted[index] == identity {
			data.accepted[index] = 0
		}
	}
}

func (data *dataPlane) isAccepted(identity uint64) bool {
	for index := range data.accepted {
		if identity != 0 && data.accepted[index] == identity {
			return true
		}
	}
	return false
}

// DataStatus is the outcome of a data-plane step.
type DataStatus uint8

const (
	DataOK DataStatus = iota
	// DataNone: no slot or no data for the socket now.
	DataNone
	// DataBroken: the service broke the ring protocol, or the session is
	// gone; the caller fails the session.
	DataBroken
)

func liveContextHeld(familyID uint64, session uint32) (*NetContext, bool) {
	context, _, found := netContextHeld(familyID)
	if !found || !context.attached || context.failed || context.endpoint.Session != session {
		return nil, false
	}
	return context, true
}

// committedHeld counts the TX slots a socket holds, filling or committed.
func (data *dataPlane) committedHeld(socket uint64) (held int, free int) {
	for index := range data.tx {
		switch {
		case data.tx[index].state == txFree:
			free++
		case data.tx[index].socket == socket:
			held++
		}
	}
	return held, free
}

// TXCredit reports whether a socket may take a TX slot now.
func TXCredit(familyID uint64, session uint32, socket uint64) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, ok := liveContextHeld(familyID, session)
	if !ok {
		return false
	}
	held, free := context.data.committedHeld(socket)
	return free != 0 && held < linuxsockapi.SocketTXSlots
}

// TXReserve takes a free TX slot for a call to fill for socket.
func TXReserve(familyID uint64, session uint32, socket uint64) (uint8, DataStatus) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, ok := liveContextHeld(familyID, session)
	if !ok {
		return 0, DataBroken
	}
	if held, _ := context.data.committedHeld(socket); held >= linuxsockapi.SocketTXSlots {
		return 0, DataNone
	}
	for index := range context.data.tx {
		slot := &context.data.tx[index]
		if slot.state == txFree {
			*slot = txSlot{state: txFilling, socket: socket}
			return uint8(index), DataOK
		}
	}
	return 0, DataNone
}

// TXAbort frees a slot a call reserved and did not fill.
func TXAbort(familyID uint64, slot uint8) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if found && int(slot) < len(context.data.tx) && context.data.tx[slot].state == txFilling {
		context.data.tx[slot] = txSlot{}
	}
}

// TXCommit commits a filled slot: length bytes, and for a datagram that
// names it, its destination. doorbell reports that the caller must send a
// Doorbell notification.
func TXCommit(familyID uint64, session uint32, rings []byte, slot uint8, length uint32, destination bool, address [4]byte, port uint16) (doorbell bool, status DataStatus) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, ok := liveContextHeld(familyID, session)
	if !ok || int(slot) >= len(context.data.tx) || context.data.tx[slot].state != txFilling {
		return false, DataBroken
	}
	data := &context.data
	data.txGen++
	entry := linuxsockapi.RingEntry{Kind: linuxsockapi.RingTX, Slot: slot, Length: length, Socket: data.tx[slot].socket, Generation: data.txGen}
	if destination {
		entry.Flags, entry.Address, entry.Port = linuxsockapi.RingDestination, address, port
	}
	if linuxsockapi.PushSubmit(rings, &entry) != linuxsockapi.RingOK {
		data.tx[slot] = txSlot{}
		return false, DataBroken
	}
	data.tx[slot].state, data.tx[slot].generation = txCommitted, data.txGen
	noteNetHeld(&netDiagnostics.RingTX)
	return linuxsockapi.ClaimDoorbell(rings), DataOK
}

// DrainComplete takes every entry of the complete ring: TX slots come back
// free, and RX slots join their socket's queue. An RX slot for a socket the
// family does not know waits for ReleaseOrphans.
func DrainComplete(familyID uint64, session uint32, rings []byte) (doorbell bool, status DataStatus) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, ok := liveContextHeld(familyID, session)
	if !ok {
		return false, DataBroken
	}
	data := &context.data
	var entry linuxsockapi.RingEntry
	for {
		switch linuxsockapi.PopComplete(rings, &entry) {
		case linuxsockapi.RingEmpty:
			return doorbell, DataOK
		case linuxsockapi.RingBroken:
			return false, DataBroken
		}
		if entry.Kind == linuxsockapi.RingTXDone {
			slot := &data.tx[entry.Slot]
			if slot.state != txCommitted || slot.generation != entry.Generation {
				return false, DataBroken
			}
			*slot = txSlot{}
			continue
		}
		slot := &data.rx[entry.Slot]
		if slot.held {
			return false, DataBroken
		}
		datagram, known := context.socketKindHeld(entry.Socket)
		known = known || data.isAccepted(entry.Socket)
		if datagram && entry.Length > linuxsockapi.MaxDatagram {
			return false, DataBroken
		}
		data.rxOrder++
		*slot = rxSlot{held: true, generation: entry.Generation, socket: entry.Socket, length: entry.Length,
			order: data.rxOrder, address: entry.Address, port: entry.Port}
		noteNetHeld(&netDiagnostics.RingRX)
		// A socket unknown now may be a child whose Accept reply still
		// waits in the channel; the next drain of the channel decides.
		slot.orphan = !known
	}
}

// ReleaseOrphans hands back the RX slots that came for a socket the family
// did not know, once the caller drained the channel after they came: by
// then any Accept reply naming the socket has been filed. doorbell reports
// that the caller must send a Doorbell notification.
func ReleaseOrphans(familyID uint64, session uint32, rings []byte) (doorbell bool, status DataStatus) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, ok := liveContextHeld(familyID, session)
	if !ok {
		return false, DataBroken
	}
	data := &context.data
	for index := range data.rx {
		slot := &data.rx[index]
		if !slot.held || !slot.orphan {
			continue
		}
		if _, known := context.socketKindHeld(slot.socket); known || data.isAccepted(slot.socket) {
			slot.orphan = false
			continue
		}
		if !data.releaseHeld(rings, uint8(index)) {
			return false, DataBroken
		}
		doorbell = true
	}
	return doorbell && linuxsockapi.ClaimDoorbell(rings), DataOK
}

// socketKindHeld finds a socket of the live session by identity.
func (context *NetContext) socketKindHeld(identity uint64) (datagram bool, found bool) {
	if socket, found := context.liveSocketHeld(identity); found {
		return socket.Datagram, true
	}
	return false, false
}

func (data *dataPlane) releaseHeld(rings []byte, slot uint8) bool {
	entry := linuxsockapi.RingEntry{Kind: linuxsockapi.RingRelease, Slot: slot, Generation: data.rx[slot].generation}
	data.rx[slot] = rxSlot{}
	return linuxsockapi.PushSubmit(rings, &entry) == linuxsockapi.RingOK
}

// RXPiece is one queued RX slot of a socket: Offset bytes of Length are
// consumed.
type RXPiece struct {
	Slot    uint8
	Offset  uint32
	Length  uint32
	Address [4]byte
	Port    uint16
}

// RXQueue copies a socket's queued RX slots, oldest first, into pieces and
// returns how many there are and the bytes they hold unread.
func RXQueue(familyID uint64, session uint32, socket uint64, pieces *[linuxsockapi.RXSlots]RXPiece) (count int, bytes uint64) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, ok := liveContextHeld(familyID, session)
	if !ok {
		return 0, 0
	}
	data := &context.data
	var orders [linuxsockapi.RXSlots]uint32
	for index := range data.rx {
		slot := &data.rx[index]
		if !slot.held || slot.socket != socket {
			continue
		}
		// Insert in ring order; orders run modulo 2^32.
		position := count
		for position > 0 && int32(slot.order-orders[position-1]) < 0 {
			pieces[position], orders[position] = pieces[position-1], orders[position-1]
			position--
		}
		pieces[position] = RXPiece{Slot: uint8(index), Offset: slot.offset, Length: slot.length, Address: slot.address, Port: slot.port}
		orders[position] = slot.order
		bytes += uint64(slot.length - slot.offset)
		count++
	}
	return count, bytes
}

// RXConsume records that a read took n more bytes of a queued slot; a slot
// read to its end, or any datagram slot, goes back to the service.
func RXConsume(familyID uint64, session uint32, rings []byte, slot uint8, n uint32, whole bool) (doorbell bool, status DataStatus) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, ok := liveContextHeld(familyID, session)
	if !ok || int(slot) >= len(context.data.rx) || !context.data.rx[slot].held {
		return false, DataBroken
	}
	held := &context.data.rx[slot]
	held.offset += min(n, held.length-held.offset)
	if !whole && held.offset < held.length {
		return false, DataOK
	}
	if !context.data.releaseHeld(rings, slot) {
		return false, DataBroken
	}
	return linuxsockapi.ClaimDoorbell(rings), DataOK
}

// RXDrop hands back every queued slot of a socket that closes.
func RXDrop(familyID uint64, session uint32, rings []byte, socket uint64) (doorbell bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, ok := liveContextHeld(familyID, session)
	if !ok {
		return false
	}
	context.data.forgetAccepted(socket)
	for index := range context.data.rx {
		if slot := &context.data.rx[index]; slot.held && slot.socket == socket {
			if !context.data.releaseHeld(rings, uint8(index)) {
				return false
			}
			doorbell = true
		}
	}
	return doorbell && linuxsockapi.ClaimDoorbell(rings)
}

// MergeReadiness adds what the kernel holds to a socket's published state:
// queued RX slots make it readable and count in Readable (for a datagram
// socket, the first queued datagram is the next one), and a socket that
// sends through TX slots is writable only with slot credit.
func MergeReadiness(familyID uint64, session uint32, datagram bool, state linuxsockapi.Readiness) linuxsockapi.Readiness {
	var pieces [linuxsockapi.RXSlots]RXPiece
	count, bytes := RXQueue(familyID, session, state.Socket, &pieces)
	if count != 0 {
		state.Ready |= linuxsockapi.ReadyIn
		if datagram {
			state.Readable = pieces[0].Length
		} else {
			state.Readable += uint32(bytes)
		}
	}
	if state.State&linuxsockapi.EntrySend != 0 && !TXCredit(familyID, session, state.Socket) {
		state.Ready &^= linuxsockapi.ReadyOut
	}
	return state
}

// LiveSessions lists the families with a live session and their endpoints,
// for the diagnostics.
func LiveSessions(families *[MaxNetFamilies]uint64, endpoints *[MaxNetFamilies]NetEndpoint) int {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	count := 0
	for index := range netContexts {
		context := &netContexts[index]
		if context.familyID != 0 && context.attached && !context.failed {
			families[count], endpoints[count] = context.familyID, context.endpoint
			count++
		}
	}
	return count
}

// DataPlaneState reports a family's slots on the kernel's side, for the
// diagnostics: TX slots being filled and committed, RX slots held.
func DataPlaneState(familyID uint64) (filling, committed, held uint8) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return 0, 0, 0
	}
	for index := range context.data.tx {
		switch context.data.tx[index].state {
		case txFilling:
			filling |= 1 << index
		case txCommitted:
			committed |= 1 << index
		}
	}
	for index := range context.data.rx {
		if context.data.rx[index].held {
			held |= 1 << index
		}
	}
	return filling, committed, held
}
