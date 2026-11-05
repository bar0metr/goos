package logpersist

import (
	"bytes"

	"goos/internal/logfmt"
)

var gapPrefix = []byte("[lost ")

// Batch is one snapshot of the ring: records First..First+Count-1, the oldest
// sequence the ring still held, and its end.
type Batch struct {
	Oldest, Total, First uint64
	Count                int
}

// Source is the kernel ring. Record reads the last batch Read returned.
type Source interface {
	Read(cursor uint64) (Batch, bool)
	Record(index int) (logfmt.Record, bool)
}

// Store is the filesystem. Paths are absolute. One file at a time is open
// for appending; Write appends to it.
type Store interface {
	// Stat reports a file's size and whether it exists; ok is false when
	// the filesystem did not answer.
	Stat(path []byte) (size uint64, exists bool, ok bool)
	ReadAt(path []byte, offset uint64, target []byte) (int, bool)
	Truncate(path []byte, size uint64) bool
	// Replace moves from onto to, removing the old to in the same step.
	Replace(from, to []byte) bool
	// OpenAppend opens path at its end, creating it when absent, and
	// returns its size.
	OpenAppend(path []byte) (uint64, bool)
	Write(data []byte) bool
	Close()
}

// Stats counts what happened to records on their way to the file. Appended
// and Overwritten are the kernel ring's own counts at the last read.
type Stats struct {
	Appended, Overwritten   uint64
	Persisted               uint64
	Lost                    uint64
	Failures                uint64
	Rotations               uint64
	RotationFailures        uint64
	RepairedBytes, Repaired uint64
}

// Outcome of a Drain.
type Outcome uint8

const (
	// CaughtUp: the cursor reached the ring's end.
	CaughtUp Outcome = iota
	// More: the pass stopped at its batch limit with records left.
	More
	// Failed: the source or the store failed; the cursor stays at the
	// first record not known to be in the file.
	Failed
)

const (
	// lineMax holds logfmt's longest line and the sequence trailer.
	lineMax = logfmt.MaxText + 32
	// tailSize holds the last whole line of a file and a torn one after it.
	tailSize = 2 * lineMax
	// chunkSize coalesces lines into one store write.
	chunkSize = 2048
	// passBatches bounds one Drain so the caller can serve its other events
	// under a sustained burst.
	passBatches = 64
	maxPath     = 64
)

// Persister owns the cursor: the sequence of the next record to write. A
// record line ends with " #N", N its sequence; a gap marker ends with the
// sequence before the first record after the gap. The last line therefore
// names the last record the file accounts for.
type Persister struct {
	source Source
	store  Store
	policy Policy
	Stats  Stats

	cursor uint64
	// lost is a gap counted but whose marker is not in the file yet.
	lost uint64
	// open is true while the live file is open; size is its length
	// including the buffered chunk.
	open bool
	size uint64
	// wrote is set once this boot tried to append, after which the file's
	// tail can describe this boot.
	wrote bool
	// recover asks the next Drain to repair the live file and, once this boot
	// wrote to it, resume after its last line: a failed write may have left
	// part of a chunk.
	recover bool
	// rotationBlocked stops retrying a failed rotation until the next Drain.
	rotationBlocked bool

	chunk        [chunkSize]byte
	chunkSize    int
	chunkRecords uint64
	chunkNext    uint64
	chunkMarker  bool

	line     [lineMax]byte
	tail     [tailSize]byte
	base     [maxPath]byte
	baseSize int
	from     [maxPath + 2]byte
	to       [maxPath + 2]byte
}

// Init prepares a persister for path; Start must run before Drain.
func (persister *Persister) Init(source Source, store Store, path string, policy Policy) bool {
	if len(path) == 0 || len(path) > maxPath || !policy.Valid() {
		return false
	}
	persister.source, persister.store, persister.policy = source, store, policy
	persister.baseSize = copy(persister.base[:], path)
	return true
}

func (persister *Persister) Cursor() uint64 { return persister.cursor }
func (persister *Persister) Policy() Policy { return persister.policy }

// SetPolicy changes the bounds from the next rotation on. An invalid policy
// is refused and the current one stays.
func (persister *Persister) SetPolicy(policy Policy) bool {
	if !policy.Valid() {
		return false
	}
	persister.policy = policy
	return true
}

// Start repairs a torn final line and places the cursor. A service started
// again in the same boot resumes after the file's last line, so it writes
// nothing twice. After a reboot the file's sequences belong to an earlier
// boot and the cursor starts at zero; records the ring dropped before the
// service started become a gap marker.
func (persister *Persister) Start(restarted bool) bool {
	batch, ok := persister.source.Read(0)
	if !ok {
		return false
	}
	persister.observe(batch)
	persister.cursor = 0
	persister.repair()
	if restarted {
		persister.wrote = true
		persister.resume(batch)
	}
	return true
}

// Skip moves the cursor to the ring's end without writing, for while
// persistence is off.
func (persister *Persister) Skip() bool {
	batch, ok := persister.source.Read(persister.cursor)
	if !ok {
		return false
	}
	persister.observe(batch)
	persister.cursor, persister.lost = batch.Total, 0
	return true
}

func (persister *Persister) observe(batch Batch) {
	persister.Stats.Appended, persister.Stats.Overwritten = batch.Total, batch.Oldest
}

// Drain writes batches until the cursor reaches the ring's end.
func (persister *Persister) Drain() Outcome {
	persister.rotationBlocked = false
	if persister.recover {
		persister.closeLive()
		persister.repair()
		if persister.wrote {
			batch, ok := persister.source.Read(persister.cursor)
			if !ok {
				return persister.fail()
			}
			persister.resume(batch)
		}
		persister.recover = false
	}
	for pass := 0; pass < passBatches; pass++ {
		batch, ok := persister.source.Read(persister.cursor)
		if !ok {
			return persister.fail()
		}
		persister.observe(batch)
		if logfmt.SequenceBefore(persister.cursor, batch.First) {
			persister.lost += batch.First - persister.cursor
			persister.Stats.Lost += batch.First - persister.cursor
			persister.cursor = batch.First
		}
		if batch.Count == 0 && persister.lost == 0 {
			persister.closeLive()
			return CaughtUp
		}
		if !persister.openLive() {
			return persister.fail()
		}
		if persister.lost != 0 && !persister.add(persister.gapLine(), persister.cursor, 0, true) {
			return persister.fail()
		}
		for index := 0; index < batch.Count; index++ {
			record, valid := persister.source.Record(index)
			if !valid {
				return persister.fail()
			}
			sequence := batch.First + uint64(index)
			if !persister.add(persister.recordLine(record, sequence), sequence+1, 1, false) {
				return persister.fail()
			}
		}
		if !persister.flush() {
			return persister.fail()
		}
		if persister.cursor == batch.Total {
			persister.closeLive()
			return CaughtUp
		}
	}
	persister.closeLive()
	return More
}

func (persister *Persister) fail() Outcome {
	persister.Stats.Failures++
	persister.closeLive()
	persister.chunkSize, persister.chunkRecords, persister.chunkMarker = 0, 0, false
	persister.recover = true
	return Failed
}

// add buffers a line, rotating first when it would carry the live file past
// its bound. next is the cursor once the line is in the file.
func (persister *Persister) add(line []byte, next uint64, records uint64, marker bool) bool {
	if persister.size != 0 && persister.size+uint64(len(line)) > persister.policy.SegmentBytes && !persister.rotationBlocked {
		if !persister.flush() {
			return false
		}
		if !persister.rotate() {
			persister.rotationBlocked = true
		}
		if !persister.openLive() {
			return false
		}
	}
	if persister.chunkSize+len(line) > len(persister.chunk) && !persister.flush() {
		return false
	}
	persister.chunkSize += copy(persister.chunk[persister.chunkSize:], line)
	persister.size += uint64(len(line))
	persister.chunkRecords += records
	persister.chunkNext = next
	persister.chunkMarker = persister.chunkMarker || marker
	return true
}

// flush writes the buffered lines and only then advances the cursor past
// them.
func (persister *Persister) flush() bool {
	if persister.chunkSize == 0 {
		return true
	}
	// A write that fails part way may leave whole lines of the chunk in the
	// file, so the recovery after it reads the tail as this boot's.
	persister.wrote = true
	if !persister.store.Write(persister.chunk[:persister.chunkSize]) {
		return false
	}
	persister.cursor = persister.chunkNext
	persister.Stats.Persisted += persister.chunkRecords
	if persister.chunkMarker {
		persister.lost = 0
	}
	persister.chunkSize, persister.chunkRecords, persister.chunkMarker = 0, 0, false
	return true
}

func (persister *Persister) openLive() bool {
	if persister.open {
		return true
	}
	size, ok := persister.store.OpenAppend(persister.base[:persister.baseSize])
	if !ok {
		return false
	}
	persister.open, persister.size = true, size
	return true
}

func (persister *Persister) closeLive() {
	if persister.open {
		persister.store.Close()
		persister.open = false
	}
}

// rotate shifts every segment up one name and the live file onto .1. Each
// step replaces its target in one operation, and the first failure stops the
// rotation, so the newest previous segment is never removed before the file
// that replaces it exists.
func (persister *Persister) rotate() bool {
	persister.closeLive()
	for index := persister.policy.Segments - 1; index >= 1; index-- {
		from := persister.segment(persister.from[:0], index-1)
		to := persister.segment(persister.to[:0], index)
		_, exists, ok := persister.store.Stat(from)
		if !ok || exists && !persister.store.Replace(from, to) {
			persister.Stats.RotationFailures++
			return false
		}
	}
	persister.Stats.Rotations++
	return true
}

// segment appends the name of segment index to target; 0 is the live file.
func (persister *Persister) segment(target []byte, index int) []byte {
	target = append(target, persister.base[:persister.baseSize]...)
	if index != 0 {
		target = append(target, '.', byte('0'+index))
	}
	return target
}

func (persister *Persister) recordLine(record logfmt.Record, sequence uint64) []byte {
	size := logfmt.Format(record, persister.line[:])
	if size == 0 {
		size = copy(persister.line[:], "[unformattable record]\n")
	}
	return persister.trailer(persister.line[:size-1], sequence)
}

// gapLine says how many records the ring overwrote before they were read.
// Its trailer is the sequence before the cursor, so a resumed service
// continues at the cursor.
func (persister *Persister) gapLine() []byte {
	text := append(persister.line[:0], "[lost "...)
	text = logfmt.AppendUint(text, persister.lost)
	text = append(text, " records]"...)
	return persister.trailer(text, persister.cursor-1)
}

func (persister *Persister) trailer(text []byte, sequence uint64) []byte {
	text = append(text, " #"...)
	text = logfmt.AppendUint(text, sequence)
	return append(text, '\n')
}

// repair cuts a final line that has no newline: a write that a crash or a
// failed request interrupted. Without a newline in the last tailSize bytes
// the whole window is cut.
func (persister *Persister) repair() {
	path := persister.base[:persister.baseSize]
	size, exists, ok := persister.store.Stat(path)
	if !ok || !exists || size == 0 {
		return
	}
	data, ok := persister.readTail(path, size)
	if !ok || data[len(data)-1] == '\n' {
		return
	}
	start := size - uint64(len(data))
	keep := start
	for index := len(data) - 1; index >= 0; index-- {
		if data[index] == '\n' {
			keep = start + uint64(index) + 1
			break
		}
	}
	if persister.store.Truncate(path, keep) {
		persister.Stats.Repaired++
		persister.Stats.RepairedBytes += size - keep
	}
}

func (persister *Persister) readTail(path []byte, size uint64) ([]byte, bool) {
	start := size - min(size, tailSize)
	count, ok := persister.store.ReadAt(path, start, persister.tail[:size-start])
	if !ok || uint64(count) != size-start || count == 0 {
		return nil, false
	}
	return persister.tail[:count], true
}

// resume moves the cursor past the last line the file holds. A sequence the
// ring still retains must match the record there, so a tail left by an
// earlier boot cannot hide this boot's records.
func (persister *Persister) resume(batch Batch) {
	last, line, found := persister.lastLine()
	if !found || !logfmt.SequenceBefore(last, batch.Total) {
		return
	}
	next := last + 1
	if !logfmt.SequenceBefore(persister.cursor, next) {
		return
	}
	if !logfmt.SequenceBefore(last, batch.Oldest) && !persister.matches(last, line) {
		return
	}
	persister.cursor, persister.lost = next, 0
}

// matches reports whether the ring's record at sequence renders as line.
func (persister *Persister) matches(sequence uint64, line []byte) bool {
	if len(line) != 0 && line[0] != '[' || bytes.HasPrefix(line, gapPrefix) {
		return true
	}
	batch, ok := persister.source.Read(sequence)
	if !ok || batch.First != sequence || batch.Count == 0 {
		return false
	}
	record, ok := persister.source.Record(0)
	if !ok {
		return false
	}
	rendered := persister.recordLine(record, sequence)
	return bytes.Equal(rendered[:len(rendered)-1], line)
}

// lastLine finds the sequence the file's last line ends with, in the live
// file or, right after a rotation, in segment 1.
func (persister *Persister) lastLine() (uint64, []byte, bool) {
	for index := 0; index < 2; index++ {
		path := persister.segment(persister.from[:0], index)
		size, exists, ok := persister.store.Stat(path)
		if !ok || !exists || size == 0 {
			continue
		}
		data, ok := persister.readTail(path, size)
		if !ok || data[len(data)-1] != '\n' {
			continue
		}
		end := len(data) - 1
		start := end
		for start > 0 && data[start-1] != '\n' {
			start--
		}
		if sequence, found := SequenceOf(data[start:end]); found {
			return sequence, data[start:end], true
		}
	}
	return 0, nil, false
}

// SequenceOf parses the " #N" trailer of a line without its newline.
func SequenceOf(line []byte) (uint64, bool) {
	end := len(line)
	start := end
	for start > 0 && line[start-1] >= '0' && line[start-1] <= '9' {
		start--
	}
	if start == end || end-start > 20 || start < 2 || line[start-1] != '#' || line[start-2] != ' ' {
		return 0, false
	}
	value := uint64(0)
	for _, digit := range line[start:end] {
		next := value*10 + uint64(digit-'0')
		if next/10 != value {
			return 0, false
		}
		value = next
	}
	return value, true
}
