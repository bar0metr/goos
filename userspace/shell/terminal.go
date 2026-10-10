package shell

import "goos/userspace/tty"

// TerminalEnvironment stores a copied terminal snapshot for child launches.
// Serial fallback dimensions are omitted; authoritative remote dimensions do
// not require GeometryDisplay. Entries borrow this storage until Store.
type TerminalEnvironment struct {
	columns     [16]byte
	columnsSize int
	lines       [16]byte
	linesSize   int
	term        [len("TERM=") + tty.MaxTerm]byte
	termSize    int
}

func (environment *TerminalEnvironment) Store(payload []byte) bool {
	environment.columnsSize, environment.linesSize, environment.termSize = 0, 0, 0
	geometry, ok := tty.DecodeGeometryState(payload)
	if !ok {
		return false
	}
	if geometry.Authoritative {
		environment.columnsSize = len(appendTerminalVariable(environment.columns[:0], "COLUMNS=", geometry.Columns))
		environment.linesSize = len(appendTerminalVariable(environment.lines[:0], "LINES=", geometry.Rows))
	}
	if len(geometry.Term) != 0 {
		environment.termSize = copy(environment.term[:], "TERM=")
		environment.termSize += copy(environment.term[environment.termSize:], geometry.Term)
	}
	return true
}

func (environment *TerminalEnvironment) Reset() {
	environment.columnsSize, environment.linesSize, environment.termSize = 0, 0, 0
}

func (environment *TerminalEnvironment) Entries(path []byte) [][]byte {
	entries := [][]byte{path}
	if environment.columnsSize != 0 && environment.linesSize != 0 {
		entries = append(entries, environment.columns[:environment.columnsSize], environment.lines[:environment.linesSize])
	}
	if environment.termSize != 0 {
		entries = append(entries, environment.term[:environment.termSize])
	}
	return entries
}

func appendTerminalVariable(target []byte, name string, value int) []byte {
	target = append(target, name...)
	var digits [5]byte
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
