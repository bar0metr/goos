package netdev

type slotState uint8

const (
	slotFree slotState = iota
	slotInUse
	slotPeer
)

// Pool is one side's view of who owns each slot of one shared pool. A slot
// is free (local, idle), in use (local, being filled or read) or held by the
// peer. Only Give and Take cross the boundary, and each matches exactly one
// protocol message, so a duplicate or orphan message shows up as a failed
// transition. The zero value owns every slot locally.
//
// Driver RX starts all local: Acquire, copy a frame, send RxReady, then Give
// on success or Release on a failed send. On RxRelease it calls Take, then
// Release. The stack's RX view starts all peer: Take on RxReady, Give once its
// RxRelease is sent. Stack TX starts all local: Acquire, write, send Tx, then
// Give or Release; on TxDone, Take then Release. Driver TX starts all peer:
// Take on Tx, Give once its TxDone is sent.
type Pool struct {
	slots [Slots]slotState
}

// Reset gives every slot to this side, free, or to the peer.
func (p *Pool) Reset(localOwnsAll bool) {
	state := slotPeer
	if localOwnsAll {
		state = slotFree
	}
	for index := range p.slots[:] {
		p.slots[index] = state
	}
}

// Acquire marks the lowest free local slot in use.
func (p *Pool) Acquire() (uint16, bool) {
	for index := range p.slots[:] {
		if p.slots[index] == slotFree {
			p.slots[index] = slotInUse
			return uint16(index), true
		}
	}
	return 0, false
}

// Release returns an in-use local slot to the free set.
func (p *Pool) Release(slot uint16) bool {
	return p.move(slot, slotInUse, slotFree)
}

// Give hands an in-use local slot to the peer. Call it only after the
// message announcing the handoff was sent; a failed send keeps the slot.
func (p *Pool) Give(slot uint16) bool {
	return p.move(slot, slotInUse, slotPeer)
}

// Take accepts a slot back from the peer as in use. It fails if the peer did
// not hold the slot, which marks a duplicate or forged message.
func (p *Pool) Take(slot uint16) bool {
	return p.move(slot, slotPeer, slotInUse)
}

// Owned reports whether this side holds slot, free or in use.
func (p *Pool) Owned(slot uint16) bool {
	return slot < Slots && p.slots[slot] != slotPeer
}

// InUse reports whether slot is held locally and not free.
func (p *Pool) InUse(slot uint16) bool {
	return slot < Slots && p.slots[slot] == slotInUse
}

// Outstanding counts the slots the peer holds.
func (p *Pool) Outstanding() int {
	count := 0
	for index := range p.slots[:] {
		if p.slots[index] == slotPeer {
			count++
		}
	}
	return count
}

func (p *Pool) move(slot uint16, from, to slotState) bool {
	if slot >= Slots || p.slots[slot] != from {
		return false
	}
	p.slots[slot] = to
	return true
}
