package linux

import (
	"encoding/binary"

	"goos/kernel/compat/linux/abi"
)

// MaxTerminals bounds the Linux terminals that exist at once. PID 1 gives
// one to each interactive Linux launch, and a native terminal session runs
// one foreground job at a time; the bound covers every local and remote
// session PID 1 serves.
const MaxTerminals = 8

const (
	// TerminalInputCapacity is what a terminal holds for readers: completed
	// lines in canonical mode, every byte otherwise. Linux has the same 4 KiB.
	TerminalInputCapacity = 4096
	// TerminalLineCapacity bounds the line being edited in canonical mode.
	// Linux keeps 4095 bytes and a newline; the line shares its buffer with
	// the completed ones there.
	TerminalLineCapacity = 4095
	// terminalSegments bounds the completed lines waiting to be read. An end
	// of file typed on an empty line is a line of no bytes.
	terminalSegments = 128
	// TerminalEchoCapacity bounds the echo one batch of input produces. Each
	// typed byte echoes at most three bytes: "^X" for a control character
	// and CR LF for a newline.
	TerminalEchoCapacity = 3 * TerminalFeedSize
	// TerminalFeedSize is the most raw input one Feed takes.
	TerminalFeedSize = 256
)

// TerminalID names a terminal record: the slot in its low half and the
// generation of the slot in its high half. A description that outlived its
// terminal names an older generation and finds nothing.
type TerminalID uint64

func terminalID(index int, generation uint32) TerminalID {
	return TerminalID(uint64(generation)<<32 | uint64(index+1))
}

func (id TerminalID) parts() (int, uint32, bool) {
	index := int(uint32(id)) - 1
	if index < 0 || index >= MaxTerminals {
		return 0, 0, false
	}
	return index, uint32(id >> 32), true
}

// Termios is the state of struct termios, kept as Linux stores it. The
// terminal acts on the bits the line discipline below implements and keeps
// every other bit as the program set it, as Linux does.
type Termios struct {
	Input, Output, Control, Local uint32
	Line                          byte
	Chars                         [abi.TermiosControlChars]byte
}

// DefaultTermios is the state of a new terminal: canonical input with echo,
// signals from the keyboard, and CR LF output for a newline. It is what a
// Linux pseudo-terminal starts with but for two input bits: IXON is clear,
// because GoOS has no output flow control behind it, and IUTF8 is set, so
// an erase removes a whole UTF-8 character.
func DefaultTermios() Termios {
	settings := Termios{
		Input:   abi.ICRNL | abi.IUTF8,
		Output:  abi.OPOST | abi.ONLCR,
		Control: abi.B38400 | abi.CS8 | abi.CREAD,
		Local:   abi.ISIG | abi.ICANON | abi.ECHO | abi.ECHOE | abi.ECHOK | abi.ECHOCTL | abi.ECHOKE | abi.IEXTEN,
	}
	settings.Chars[abi.VINTR] = 0x03
	settings.Chars[abi.VQUIT] = 0x1c
	settings.Chars[abi.VERASE] = 0x7f
	settings.Chars[abi.VKILL] = 0x15
	settings.Chars[abi.VEOF] = 0x04
	settings.Chars[abi.VMIN] = 1
	settings.Chars[abi.VSTART] = 0x11
	settings.Chars[abi.VSTOP] = 0x13
	settings.Chars[abi.VSUSP] = 0x1a
	settings.Chars[abi.VREPRINT] = 0x12
	settings.Chars[abi.VDISCARD] = 0x0f
	settings.Chars[abi.VWERASE] = 0x17
	settings.Chars[abi.VLNEXT] = 0x16
	return settings
}

// Encode writes the kernel's struct termios. Field by field: the wire layout
// is the C one, never a Go struct's.
func (settings *Termios) Encode(wire *[abi.TermiosSize]byte) {
	binary.LittleEndian.PutUint32(wire[abi.TermiosInputOffset:], settings.Input)
	binary.LittleEndian.PutUint32(wire[abi.TermiosOutputOffset:], settings.Output)
	binary.LittleEndian.PutUint32(wire[abi.TermiosControlOffset:], settings.Control)
	binary.LittleEndian.PutUint32(wire[abi.TermiosLocalOffset:], settings.Local)
	wire[abi.TermiosLineOffset] = settings.Line
	copy(wire[abi.TermiosCharsOffset:], settings.Chars[:])
}

// DecodeTermios reads a copied struct termios.
func DecodeTermios(wire *[abi.TermiosSize]byte) Termios {
	settings := Termios{
		Input:   binary.LittleEndian.Uint32(wire[abi.TermiosInputOffset:]),
		Output:  binary.LittleEndian.Uint32(wire[abi.TermiosOutputOffset:]),
		Control: binary.LittleEndian.Uint32(wire[abi.TermiosControlOffset:]),
		Local:   binary.LittleEndian.Uint32(wire[abi.TermiosLocalOffset:]),
		Line:    wire[abi.TermiosLineOffset],
	}
	copy(settings.Chars[:], wire[abi.TermiosCharsOffset:])
	return settings
}

// TerminalSignals is the set of signals one batch of input asks for: bit
// n-1 for signal n.
type TerminalSignals uint64

func (signals TerminalSignals) Has(signal uint64) bool {
	return signal != 0 && signal <= 64 && signals&(1<<(signal-1)) != 0
}

// Echo collects the bytes the line discipline writes back to the terminal
// while it takes input. They go out with output processing applied.
type Echo struct {
	data [TerminalEchoCapacity]byte
	size int
}

func (echo *Echo) Bytes() []byte { return echo.data[:echo.size] }
func (echo *Echo) Reset()        { echo.size = 0 }

func (echo *Echo) put(values ...byte) {
	for _, value := range values {
		if echo.size < len(echo.data) {
			echo.data[echo.size] = value
			echo.size++
		}
	}
}

// Discipline is the Linux line discipline of one terminal: the termios
// state, the line being edited, and the input waiting for readers. It has no
// lock and no stream; the terminal table owns both.
type Discipline struct {
	Settings Termios
	line     [TerminalLineCapacity]byte
	lineSize int
	// ready is a ring of bytes readers may take. In canonical mode they
	// take one segment at a time; a segment is a completed line, or what a
	// change to non-canonical mode left of the line being edited.
	ready     [TerminalInputCapacity]byte
	readyHead int
	readySize int
	segments  [terminalSegments]uint16
	segHead   int
	segCount  int
	segOpen   bool
}

// Reset gives the discipline its default state with nothing waiting, field by
// field: a composite literal of this size is a kernel heap allocation.
func (discipline *Discipline) Reset() {
	discipline.Settings = DefaultTermios()
	clear(discipline.line[:])
	clear(discipline.ready[:])
	clear(discipline.segments[:])
	discipline.lineSize, discipline.readyHead, discipline.readySize = 0, 0, 0
	discipline.segHead, discipline.segCount, discipline.segOpen = 0, 0, false
}

func (discipline *Discipline) canonical() bool {
	return discipline.Settings.Local&abi.ICANON != 0
}

func (discipline *Discipline) echoing() bool {
	return discipline.Settings.Local&abi.ECHO != 0
}

// Apply installs new settings. Leaving canonical mode makes the line being
// edited readable as it stands, as Linux does.
func (discipline *Discipline) Apply(settings Termios) {
	wasCanonical := discipline.canonical()
	discipline.Settings = settings
	if wasCanonical && !discipline.canonical() && discipline.lineSize != 0 {
		discipline.pushLine(discipline.line[:discipline.lineSize], false)
		discipline.lineSize = 0
	}
}

// Flush discards input waiting for readers and the line being edited.
func (discipline *Discipline) Flush() {
	discipline.readyHead, discipline.readySize = 0, 0
	discipline.segHead, discipline.segCount, discipline.segOpen = 0, 0, false
	discipline.lineSize = 0
}

// Pending returns how many bytes a reader could take now, as FIONREAD
// reports them.
func (discipline *Discipline) Pending() int {
	if !discipline.canonical() {
		return discipline.readySize
	}
	if discipline.segCount == 0 {
		return 0
	}
	return discipline.readySize
}

// Readable reports whether a read would return without waiting, wanted
// being the bytes it asks for. A canonical read waits for a completed line;
// a non-canonical one for VMIN bytes, or none when VMIN is zero.
func (discipline *Discipline) Readable(wanted int) bool {
	if discipline.canonical() {
		return discipline.segCount != 0
	}
	minimum := int(discipline.Settings.Chars[abi.VMIN])
	if minimum == 0 {
		return true
	}
	if wanted > 0 && minimum > wanted {
		minimum = wanted
	}
	return discipline.readySize >= minimum
}

// Read takes input into target. In canonical mode it takes at most the rest
// of one line; an end of file typed on an empty line is a read of zero bytes.
func (discipline *Discipline) Read(target []byte) int {
	if len(target) == 0 {
		return 0
	}
	if discipline.canonical() {
		if discipline.segCount == 0 {
			return 0
		}
		length := int(discipline.segments[discipline.segHead])
		count := length
		if count > len(target) {
			count = len(target)
		}
		discipline.take(target[:count])
		if count == length {
			discipline.popSegment()
		} else {
			discipline.segments[discipline.segHead] = uint16(length - count)
		}
		return count
	}
	count := discipline.readySize
	if count > len(target) {
		count = len(target)
	}
	discipline.take(target[:count])
	discipline.consumeSegments(count)
	return count
}

func (discipline *Discipline) take(target []byte) {
	for index := range target {
		target[index] = discipline.ready[(discipline.readyHead+index)%len(discipline.ready)]
	}
	discipline.readyHead = (discipline.readyHead + len(target)) % len(discipline.ready)
	discipline.readySize -= len(target)
}

func (discipline *Discipline) popSegment() {
	discipline.segHead = (discipline.segHead + 1) % len(discipline.segments)
	discipline.segCount--
	if discipline.segCount == 0 {
		discipline.segOpen = false
	}
}

// consumeSegments keeps the segment lengths in step with a non-canonical
// read, which ignores where lines end.
func (discipline *Discipline) consumeSegments(count int) {
	for count > 0 && discipline.segCount > 0 {
		length := int(discipline.segments[discipline.segHead])
		if length > count {
			discipline.segments[discipline.segHead] = uint16(length - count)
			return
		}
		count -= length
		discipline.popSegment()
	}
}

// pushLine makes bytes readable as one segment. A segment of no bytes is an
// end of file. Bytes that do not fit are dropped, as Linux drops input once
// its buffer is full; a line is never split.
func (discipline *Discipline) pushLine(bytes []byte, complete bool) bool {
	if discipline.segCount == len(discipline.segments) || len(bytes) > len(discipline.ready)-discipline.readySize {
		return false
	}
	for _, value := range bytes {
		discipline.ready[(discipline.readyHead+discipline.readySize)%len(discipline.ready)] = value
		discipline.readySize++
	}
	tail := (discipline.segHead + discipline.segCount) % len(discipline.segments)
	discipline.segments[tail] = uint16(len(bytes))
	discipline.segCount++
	discipline.segOpen = !complete
	return true
}

// pushRaw makes one byte readable in non-canonical mode.
func (discipline *Discipline) pushRaw(value byte) bool {
	if discipline.readySize == len(discipline.ready) {
		return false
	}
	discipline.ready[(discipline.readyHead+discipline.readySize)%len(discipline.ready)] = value
	discipline.readySize++
	if discipline.segOpen && discipline.segCount > 0 {
		last := (discipline.segHead + discipline.segCount - 1) % len(discipline.segments)
		discipline.segments[last]++
		return true
	}
	if discipline.segCount == len(discipline.segments) {
		// Non-canonical bytes need no boundaries; the oldest segment
		// absorbs them so the byte is still counted once.
		discipline.segments[discipline.segHead]++
		return true
	}
	tail := (discipline.segHead + discipline.segCount) % len(discipline.segments)
	discipline.segments[tail] = 1
	discipline.segCount++
	discipline.segOpen = true
	return true
}

// Feed processes typed bytes: input translation, the signal characters,
// line editing in canonical mode, and echo. It returns the signals the
// input asks for. A signal character discards the input waiting, unless
// NOFLSH says otherwise, as it does on Linux.
func (discipline *Discipline) Feed(raw []byte, echo *Echo) TerminalSignals {
	var signals TerminalSignals
	settings := &discipline.Settings
	for _, value := range raw {
		if settings.Input&abi.ISTRIP != 0 {
			value &= 0x7f
		}
		switch {
		case value == '\r' && settings.Input&abi.IGNCR != 0:
			continue
		case value == '\r' && settings.Input&abi.ICRNL != 0:
			value = '\n'
		case value == '\n' && settings.Input&abi.INLCR != 0:
			value = '\r'
		}
		if settings.Local&abi.ISIG != 0 {
			if signal := discipline.signalFor(value); signal != 0 {
				signals |= 1 << (signal - 1)
				if settings.Local&abi.NOFLSH == 0 {
					discipline.Flush()
				}
				if discipline.echoing() {
					discipline.echoControl(value, echo)
				}
				continue
			}
		}
		if !discipline.canonical() {
			if discipline.pushRaw(value) && discipline.echoing() {
				discipline.echoByte(value, echo)
			}
			continue
		}
		discipline.edit(value, echo)
	}
	return signals
}

func (discipline *Discipline) signalFor(value byte) uint64 {
	chars := &discipline.Settings.Chars
	switch {
	case value == 0:
		return 0
	case value == chars[abi.VINTR]:
		return abi.SignalInterrupt
	case value == chars[abi.VQUIT]:
		return abi.SignalQuit
	case value == chars[abi.VSUSP]:
		return abi.SignalTermStop
	}
	return 0
}

// edit applies one byte to the line being edited in canonical mode.
func (discipline *Discipline) edit(value byte, echo *Echo) {
	settings := &discipline.Settings
	chars := &settings.Chars
	switch {
	case value != 0 && value == chars[abi.VERASE]:
		discipline.erase(echo)
		return
	case value != 0 && value == chars[abi.VWERASE] && settings.Local&abi.IEXTEN != 0:
		for discipline.lineSize > 0 && discipline.line[discipline.lineSize-1] == ' ' {
			discipline.erase(echo)
		}
		for discipline.lineSize > 0 && discipline.line[discipline.lineSize-1] != ' ' {
			discipline.erase(echo)
		}
		return
	case value != 0 && value == chars[abi.VKILL]:
		if discipline.echoing() && settings.Local&abi.ECHOKE != 0 && settings.Local&abi.ECHOE != 0 {
			for discipline.lineSize > 0 {
				discipline.erase(echo)
			}
			return
		}
		discipline.lineSize = 0
		if discipline.echoing() {
			discipline.echoControl(value, echo)
			if settings.Local&abi.ECHOK != 0 {
				echo.put('\n')
			}
		}
		return
	case value != 0 && value == chars[abi.VEOF]:
		// The line goes to readers without the end-of-file character; on
		// an empty line that is a read of nothing.
		if discipline.pushLine(discipline.line[:discipline.lineSize], true) {
			discipline.lineSize = 0
		}
		return
	case value == '\n' || value != 0 && (value == chars[abi.VEOL] || value == chars[abi.VEOL2] && settings.Local&abi.IEXTEN != 0):
		if discipline.lineSize == len(discipline.line) {
			discipline.lineSize--
		}
		discipline.line[discipline.lineSize] = value
		if discipline.pushLine(discipline.line[:discipline.lineSize+1], true) {
			discipline.lineSize = 0
			if discipline.echoing() || value == '\n' && settings.Local&abi.ECHONL != 0 {
				discipline.echoByte(value, echo)
			}
		}
		return
	}
	if discipline.lineSize == len(discipline.line) {
		// Linux keeps the last byte of a full line for the newline.
		return
	}
	discipline.line[discipline.lineSize] = value
	discipline.lineSize++
	if discipline.echoing() {
		discipline.echoByte(value, echo)
	}
}

// erase removes the last character of the line: one byte, or with IUTF8
// every continuation byte of a UTF-8 sequence and its lead byte.
func (discipline *Discipline) erase(echo *Echo) {
	if discipline.lineSize == 0 {
		return
	}
	settings := &discipline.Settings
	last := discipline.line[discipline.lineSize-1]
	discipline.lineSize--
	if settings.Input&abi.IUTF8 != 0 {
		for discipline.lineSize > 0 && last&0xc0 == 0x80 {
			last = discipline.line[discipline.lineSize-1]
			discipline.lineSize--
		}
	}
	if !discipline.echoing() {
		return
	}
	if settings.Local&abi.ECHOE == 0 {
		discipline.echoControl(settings.Chars[abi.VERASE], echo)
		return
	}
	width := 1
	if last < 0x20 && last != '\t' && settings.Local&abi.ECHOCTL != 0 || last == 0x7f && settings.Local&abi.ECHOCTL != 0 {
		width = 2
	}
	for ; width > 0; width-- {
		echo.put('\b', ' ', '\b')
	}
}

// echoByte echoes one byte as typed: a control character other than tab and
// newline as ^X under ECHOCTL.
func (discipline *Discipline) echoByte(value byte, echo *Echo) {
	if value == '\n' || value == '\t' || value >= 0x20 && value != 0x7f || discipline.Settings.Local&abi.ECHOCTL == 0 {
		echo.put(value)
		return
	}
	discipline.echoControl(value, echo)
}

func (discipline *Discipline) echoControl(value byte, echo *Echo) {
	if discipline.Settings.Local&abi.ECHOCTL == 0 {
		echo.put(value)
		return
	}
	echo.put('^', value^0x40)
}

// OutputSize returns how many bytes of source fit target once output
// processing has expanded them, and how many target bytes they become.
// A newline becomes CR LF under OPOST and ONLCR; nothing else changes, so
// escape sequences and UTF-8 pass as written.
func (settings *Termios) OutputSize(source []byte, room int) (consumed int, produced int) {
	expand := settings.Output&abi.OPOST != 0 && settings.Output&abi.ONLCR != 0
	for consumed < len(source) {
		width := 1
		if expand && source[consumed] == '\n' {
			width = 2
		}
		if produced+width > room {
			break
		}
		produced += width
		consumed++
	}
	return consumed, produced
}

// TransformOutput applies output processing to source and writes the result
// to target, which must hold what OutputSize reported.
func (settings *Termios) TransformOutput(source []byte, target []byte) int {
	expand := settings.Output&abi.OPOST != 0 && settings.Output&abi.ONLCR != 0
	size := 0
	for _, value := range source {
		if expand && value == '\n' {
			target[size] = '\r'
			size++
		}
		target[size] = value
		size++
	}
	return size
}
