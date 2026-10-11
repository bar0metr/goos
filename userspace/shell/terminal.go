package shell

import "goos/userspace/tty"

// TerminalEnvironment stores a copied terminal snapshot for child launches.
// Serial fallback dimensions are omitted; authoritative remote dimensions do
// not require GeometryDisplay. Entries borrow this storage until Store.
type TerminalEnvironment struct {
	snapshot          tty.Snapshot
	sequence          [48]byte
	sequenceSize      int
	display           [32]byte
	displaySize       int
	authoritative     [40]byte
	authoritativeSize int
	columns           [16]byte
	columnsSize       int
	lines             [16]byte
	linesSize         int
	term              [len("TERM=") + tty.MaxTerm]byte
	termSize          int
}

func (environment *TerminalEnvironment) Store(payload []byte) bool {
	geometry, ok := tty.DecodeGeometryState(payload)
	if !ok || !environment.snapshot.Apply(geometry) {
		return false
	}
	environment.columnsSize, environment.linesSize, environment.termSize = 0, 0, 0
	environment.sequenceSize, environment.displaySize, environment.authoritativeSize = 0, 0, 0
	if geometry.Authoritative {
		environment.columnsSize = len(appendTerminalVariable(environment.columns[:0], "COLUMNS=", uint64(geometry.Columns)))
		environment.linesSize = len(appendTerminalVariable(environment.lines[:0], "LINES=", uint64(geometry.Rows)))
	}
	if geometry.Authoritative {
		environment.sequenceSize = len(appendTerminalVariable(environment.sequence[:0], tty.SequenceVariable+"=", geometry.Sequence))
		display := uint64(0)
		if geometry.Display {
			display = 1
		}
		environment.displaySize = len(appendTerminalVariable(environment.display[:0], tty.DisplayVariable+"=", display))
		environment.authoritativeSize = len(appendTerminalVariable(environment.authoritative[:0], tty.AuthoritativeVariable+"=", 1))
	}
	if len(geometry.Term) != 0 {
		environment.termSize = copy(environment.term[:], "TERM=")
		environment.termSize += copy(environment.term[environment.termSize:], geometry.Term)
	}
	return true
}

func (environment *TerminalEnvironment) Reset() {
	*environment = TerminalEnvironment{}
}

func (environment *TerminalEnvironment) Entries(path []byte) [][]byte {
	entries := [][]byte{}
	if len(path) != 0 {
		entries = append(entries, path)
	}
	if environment.columnsSize != 0 && environment.linesSize != 0 {
		entries = append(entries, environment.columns[:environment.columnsSize], environment.lines[:environment.linesSize])
	}
	if environment.termSize != 0 {
		entries = append(entries, environment.term[:environment.termSize])
	}
	if environment.sequenceSize != 0 {
		entries = append(entries, environment.sequence[:environment.sequenceSize], environment.display[:environment.displaySize], environment.authoritative[:environment.authoritativeSize])
	}
	return entries
}

func appendTerminalVariable(target []byte, name string, value uint64) []byte {
	target = append(target, name...)
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte(value%10) + '0'
		value /= 10
		if value == 0 {
			break
		}
	}
	return append(target, digits[position:]...)
}
