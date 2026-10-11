package console

// serialNewlines keeps CRLF intact across separate console writes.
type serialNewlines struct{ previousCR bool }

func (state *serialNewlines) insertCR(item byte) bool {
	insert := item == '\n' && !state.previousCR
	state.previousCR = item == '\r'
	return insert
}
