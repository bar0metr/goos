package linux

import "goos/kernel/compat/linux/abi"

// TerminalEndpoints are the native stream ends a terminal holds: what the
// keyboard sends, where its output and error output go, and the control
// stream PID 1 writes geometry to. They are kernel stream endpoint objects,
// never handle IDs, and no Linux call returns them.
type TerminalEndpoints struct {
	Input, Output, Error, Control uint64
}

// terminalRecord is one Linux terminal. A record is live from the launch
// that creates it until its last reference goes: one per open-file
// description that names it, and one while a session holds it as its
// controlling terminal.
type terminalRecord struct {
	generation uint32
	live       bool
	familyID   uint64
	references uint16
	endpoints  TerminalEndpoints
	discipline Discipline
	rows       uint16
	columns    uint16
	// session is the SID of the session the terminal controls, or zero once
	// that session has gone. foreground is the process group terminal input
	// signals go to.
	session    int32
	foreground int32
	hangup     bool
	stats      TerminalStats
}

// TerminalStats counts what a terminal did, for diagnostics.
type TerminalStats struct {
	Interrupts, Suspends, Quits, Resizes uint64
	// LastUnsupported is the last ioctl request the terminal refused.
	LastUnsupported uint64
}

var terminals [MaxTerminals]terminalRecord

func (id TerminalID) recordHeld() (*terminalRecord, bool) {
	guard.Check()
	index, generation, valid := id.parts()
	if !valid {
		return nil, false
	}
	record := &terminals[index]
	if !record.live || record.generation != generation {
		return nil, false
	}
	return record, true
}

// TerminalSnapshot is what a terminal call reads of a record in one step.
type TerminalSnapshot struct {
	FamilyID   uint64
	Endpoints  TerminalEndpoints
	Settings   Termios
	Rows       uint16
	Columns    uint16
	Session    int32
	Foreground int32
	Hangup     bool
	Stats      TerminalStats
}

func (record *terminalRecord) snapshot() TerminalSnapshot {
	return TerminalSnapshot{FamilyID: record.familyID, Endpoints: record.endpoints, Settings: record.discipline.Settings,
		Rows: record.rows, Columns: record.columns, Session: record.session, Foreground: record.foreground,
		Hangup: record.hangup, Stats: record.stats}
}

func createTerminalHeld(familyID uint64, endpoints TerminalEndpoints, rows, columns uint16, session int32) (TerminalID, bool) {
	guard.Check()
	if familyID == 0 || endpoints.Input == 0 || endpoints.Output == 0 || endpoints.Error == 0 || endpoints.Control == 0 || session < firstPID {
		return 0, false
	}
	for index := range terminals {
		record := &terminals[index]
		if record.live {
			continue
		}
		generation := record.generation + 1
		if generation == 0 {
			generation = 1
		}
		record.generation, record.live, record.familyID, record.references = generation, true, familyID, 1
		record.endpoints, record.rows, record.columns = endpoints, rows, columns
		record.session, record.foreground, record.hangup = session, session, false
		record.stats = TerminalStats{}
		record.discipline.Reset()
		return terminalID(index, generation), true
	}
	return 0, false
}

func retainTerminalHeld(id TerminalID) bool {
	record, found := id.recordHeld()
	if !found || record.references == ^uint16(0) {
		return false
	}
	record.references++
	return true
}

func releaseTerminalHeld(id TerminalID) (TerminalEndpoints, bool) {
	record, found := id.recordHeld()
	if !found || record.references == 0 {
		return TerminalEndpoints{}, false
	}
	record.references--
	if record.references != 0 {
		return TerminalEndpoints{}, false
	}
	endpoints := record.endpoints
	// The slot keeps its generation, so a stale identity never names the
	// record that reuses it.
	record.live, record.familyID, record.references = false, 0, 0
	record.endpoints, record.rows, record.columns = TerminalEndpoints{}, 0, 0
	record.session, record.foreground, record.hangup = 0, 0, false
	record.stats = TerminalStats{}
	record.discipline.Reset()
	return endpoints, true
}

// detachSessionHeld ends the controlling-terminal link of a session whose
// leader exited. The reference the session held is added to released for the
// caller to give up outside the tables. The foreground group loses the
// terminal's signals: no session owns it any more.
func detachSessionHeld(session int32, released *Released) {
	guard.Check()
	for index := range terminals {
		record := &terminals[index]
		if !record.live || record.session != session || session == 0 {
			continue
		}
		record.session, record.foreground = 0, 0
		released.add(FileDescription{Kind: FDTerminal, Flags: abi.OpenReadWrite, Object: uint64(terminalID(index, record.generation))})
	}
}

// controllingTerminalHeld returns the terminal session controls.
func controllingTerminalHeld(session int32) (TerminalID, bool) {
	guard.Check()
	if session < firstPID {
		return 0, false
	}
	for index := range terminals {
		if record := &terminals[index]; record.live && record.session == session {
			return terminalID(index, record.generation), true
		}
	}
	return 0, false
}

// CreateTerminal makes the terminal of an interactive launch, controlled by
// the session the launch root leads. It holds the session's reference.
func CreateTerminal(familyID uint64, endpoints TerminalEndpoints, rows, columns uint16, session int32) (TerminalID, bool) {
	tablesLock.Lock()
	id, ok := createTerminalHeld(familyID, endpoints, rows, columns, session)
	tablesLock.Unlock()
	return id, ok
}

// RetainTerminal adds a reference for a new description.
func RetainTerminal(id TerminalID) bool {
	tablesLock.Lock()
	ok := retainTerminalHeld(id)
	tablesLock.Unlock()
	return ok
}

// ReleaseTerminal gives up one reference. With the last one it returns the
// endpoints the record held, which the caller releases.
func ReleaseTerminal(id TerminalID) (TerminalEndpoints, bool) {
	tablesLock.Lock()
	endpoints, last := releaseTerminalHeld(id)
	tablesLock.Unlock()
	return endpoints, last
}

// Terminal returns a snapshot of a live terminal.
func Terminal(id TerminalID) (TerminalSnapshot, bool) {
	tablesLock.Lock()
	record, found := id.recordHeld()
	var snapshot TerminalSnapshot
	if found {
		snapshot = record.snapshot()
	}
	tablesLock.Unlock()
	return snapshot, found
}

// TerminalAt returns the terminal in slot index, for the kernel's scan of
// terminals with input nobody has taken yet.
func TerminalAt(index int) (TerminalID, TerminalSnapshot, bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	if index < 0 || index >= MaxTerminals || !terminals[index].live {
		return 0, TerminalSnapshot{}, false
	}
	record := &terminals[index]
	return terminalID(index, record.generation), record.snapshot(), true
}

// ControllingTerminal returns the terminal a session controls.
func ControllingTerminal(session int32) (TerminalID, bool) {
	tablesLock.Lock()
	id, found := controllingTerminalHeld(session)
	tablesLock.Unlock()
	return id, found
}

// FeedTerminal runs typed bytes through the line discipline. echo receives
// what the terminal shows back. It returns the signals the input generated
// and the record as it is afterwards, whose foreground group receives them.
func FeedTerminal(id TerminalID, raw []byte, echo *Echo) (TerminalSignals, TerminalSnapshot, bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	record, found := id.recordHeld()
	if !found {
		return 0, TerminalSnapshot{}, false
	}
	signals := record.discipline.Feed(raw, echo)
	if signals.Has(abi.SignalInterrupt) {
		record.stats.Interrupts++
	}
	if signals.Has(abi.SignalTermStop) {
		record.stats.Suspends++
	}
	if signals.Has(abi.SignalQuit) {
		record.stats.Quits++
	}
	return signals, record.snapshot(), true
}

// ReadTerminal takes processed input into target. ready reports that the
// read is complete as it stands: bytes were taken, a canonical end of file
// was read, VMIN is zero, or the terminal hung up. Otherwise the caller
// waits.
func ReadTerminal(id TerminalID, target []byte) (count int, ready bool, found bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	record, found := id.recordHeld()
	if !found {
		return 0, false, false
	}
	if record.discipline.Readable(len(target)) {
		return record.discipline.Read(target), true, true
	}
	return 0, record.hangup, true
}

// TerminalReadable reports whether a read of wanted bytes would complete
// without waiting.
func TerminalReadable(id TerminalID, wanted int) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	record, found := id.recordHeld()
	return found && (record.hangup || record.discipline.Readable(wanted))
}

// TerminalPending reports the bytes FIONREAD counts.
func TerminalPending(id TerminalID) int {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	if record, found := id.recordHeld(); found {
		return record.discipline.Pending()
	}
	return 0
}

// SetTerminalSettings installs termios state. flush discards input first,
// as TCSETSF does.
func SetTerminalSettings(id TerminalID, settings Termios, flush bool) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	record, found := id.recordHeld()
	if !found {
		return false
	}
	if flush {
		record.discipline.Flush()
	}
	record.discipline.Apply(settings)
	return true
}

// FlushTerminalInput discards input waiting for readers, as TCFLSH does
// for the input queue.
func FlushTerminalInput(id TerminalID) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	record, found := id.recordHeld()
	if found {
		record.discipline.Flush()
	}
	return found
}

// ResizeTerminal records new geometry. It reports whether the size changed,
// and the foreground group that SIGWINCH then goes to.
func ResizeTerminal(id TerminalID, rows, columns uint16) (bool, TerminalSnapshot) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	record, found := id.recordHeld()
	if !found || rows == 0 || columns == 0 || record.rows == rows && record.columns == columns {
		return false, TerminalSnapshot{}
	}
	record.rows, record.columns = rows, columns
	record.stats.Resizes++
	return true, record.snapshot()
}

// HangUpTerminal records that the native session behind the terminal has
// gone: reads return end of file from now on.
func HangUpTerminal(id TerminalID) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	record, found := id.recordHeld()
	if !found || record.hangup {
		return false
	}
	record.hangup = true
	return true
}

// NoteTerminalIoctl records a request a terminal refused.
func NoteTerminalIoctl(id TerminalID, request uint64) {
	tablesLock.Lock()
	if record, found := id.recordHeld(); found {
		record.stats.LastUnsupported = request
	}
	tablesLock.Unlock()
}

func setTerminalForegroundHeld(id TerminalID, callerPID int32, group int32) abi.Errno {
	record, found := id.recordHeld()
	if !found {
		return abi.ENOTTY
	}
	caller, callerFound := findProcessRecordHeld(callerPID)
	if !callerFound || record.session == 0 || caller.SID != record.session {
		return abi.ENOTTY
	}
	if group <= 0 {
		return abi.EINVAL
	}
	// The group must exist in the terminal's session.
	for index := range processRecords {
		member := &processRecords[index]
		if member.State == ProcessRunning && member.PGID == group {
			if member.SID != record.session || member.FamilyID != record.familyID {
				return abi.EPERM
			}
			record.foreground = group
			return 0
		}
	}
	return abi.ESRCH
}

// SetTerminalForeground makes group the foreground process group, as
// TIOCSPGRP does. The caller must belong to the session the terminal
// controls, and group to that session.
func SetTerminalForeground(id TerminalID, callerPID int32, group int32) abi.Errno {
	tablesLock.Lock()
	errno := setTerminalForegroundHeld(id, callerPID, group)
	tablesLock.Unlock()
	return errno
}

// TerminalStatus describes a terminal as fstat reports it: a character
// device that only its user reads and writes, numbered as Linux numbers a
// pseudo-terminal, major 136 with the slot as minor.
func TerminalStatus(id TerminalID) FileStatus {
	index, _, _ := id.parts()
	number := uint64(136)<<8 | uint64(index)
	return FileStatus{Device: DeviceFilesystem, Inode: 0x200 + uint64(index), Mode: abi.ModeCharacter | 0o620, RawDevice: number}
}
