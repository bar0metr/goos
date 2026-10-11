package logfmt

// SequenceBefore compares wrapping uint64 log cursors. The retained window is
// 64 records; a reader must remain within half the sequence space of the writer
// (2^63 appends) for its position to stay unambiguous.
func SequenceBefore(left, right uint64) bool { return int64(left-right) < 0 }
