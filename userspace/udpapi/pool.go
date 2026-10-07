package udpapi

// Slots tracks which side holds each slot of one pool, as seen from one
// side. Every handoff in the protocol is one call: Lend when this side's
// message hands a slot over, Return when the peer's message hands it back.
// Return fails for a slot this side did not lend, which marks a duplicate or
// forged message.
type Slots struct {
	lent  uint8
	count uint8
}

func (s *Slots) bit(slot uint8) (uint8, bool) {
	if slot >= s.count || slot >= 8 {
		return 0, false
	}
	return 1 << slot, true
}

// Reset makes count slots, all held by this side.
func (s *Slots) Reset(count uint8) {
	s.lent, s.count = 0, count
}

// Free returns the lowest slot this side holds, or false.
func (s *Slots) Free() (uint8, bool) {
	for slot := uint8(0); slot < s.count && slot < 8; slot++ {
		if s.lent&(1<<slot) == 0 {
			return slot, true
		}
	}
	return 0, false
}

// Lend hands a held slot to the peer.
func (s *Slots) Lend(slot uint8) bool {
	bit, ok := s.bit(slot)
	if !ok || s.lent&bit != 0 {
		return false
	}
	s.lent |= bit
	return true
}

// Return takes back a slot the peer holds.
func (s *Slots) Return(slot uint8) bool {
	bit, ok := s.bit(slot)
	if !ok || s.lent&bit == 0 {
		return false
	}
	s.lent &^= bit
	return true
}

// Lent reports whether the peer holds slot.
func (s *Slots) Lent(slot uint8) bool {
	bit, ok := s.bit(slot)
	return ok && s.lent&bit != 0
}

// Offset is where slot starts in a pool.
func Offset(slot uint8) uint64 { return uint64(slot) * SlotSize }
