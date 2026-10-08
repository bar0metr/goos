package tcpapi

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

func (s *Slots) Reset(count uint8) { s.lent, s.count = 0, count }

func (s *Slots) Free() (uint8, bool) {
	for slot := uint8(0); slot < s.count && slot < 8; slot++ {
		if s.lent&(1<<slot) == 0 {
			return slot, true
		}
	}
	return 0, false
}

func (s *Slots) Lend(slot uint8) bool {
	bit, ok := s.bit(slot)
	if !ok || s.lent&bit != 0 {
		return false
	}
	s.lent |= bit
	return true
}

func (s *Slots) Return(slot uint8) bool {
	bit, ok := s.bit(slot)
	if !ok || s.lent&bit == 0 {
		return false
	}
	s.lent &^= bit
	return true
}

func (s *Slots) Lent(slot uint8) bool {
	bit, ok := s.bit(slot)
	return ok && s.lent&bit != 0
}

func Offset(slot uint8) uint64 { return uint64(slot) * SlotSize }
