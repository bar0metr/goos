package tty

// A terminal on COM1 is resized without telling anyone. A native full-screen
// program asks it for its size while idle; a Linux program expects the kernel
// to know, so the service asks on its behalf while a session is in transport
// mode (ADR 0206). The answer never reaches the program.

// probeSequence saves the cursor, asks where the bottom-right corner is, and
// restores the cursor. The terminal handles it as one write between two of
// the program's sequences, so the program's drawing is undisturbed.
const probeSequence = "\x1b7\x1b[999;999H\x1b[6n\x1b8"

// A terminal that leaves this many probes unanswered is not asked again until
// the next transport session: it cannot answer, and the bytes are noise.
const probeUnanswered = 3

// Output sequence states; the probe waits for a byte boundary.
const (
	outputGround uint8 = iota
	outputEscape
	outputControl
	outputString
	outputStringEscape
)

type sizeProbe struct {
	// awaiting holds input back that may be the answer; it ends with the
	// answer or at the next tick.
	awaiting   bool
	resting    bool
	unanswered int
	held       [16]byte
	heldSize   int
	state      uint8
	utf8       uint8
}

func (probe *sizeProbe) begin() { probe.resting, probe.unanswered = false, 0 }

// track follows program output to know whether it stopped inside a sequence
// or a UTF-8 character.
func (probe *sizeProbe) track(data []byte) {
	for _, value := range data {
		switch probe.state {
		case outputEscape:
			switch {
			case value == '[':
				probe.state = outputControl
			case value == ']' || value == 'P' || value == '_' || value == '^' || value == 'X':
				probe.state = outputString
			case value < 0x20 || value > 0x2f:
				probe.state = outputGround
			}
		case outputControl:
			if value >= 0x40 && value <= 0x7e {
				probe.state = outputGround
			}
		case outputString:
			if value == 0x07 {
				probe.state = outputGround
			} else if value == 0x1b {
				probe.state = outputStringEscape
			}
		case outputStringEscape:
			probe.state = outputGround
		default:
			switch {
			case value == 0x1b:
				probe.state, probe.utf8 = outputEscape, 0
			case value >= 0xf0:
				probe.utf8 = 3
			case value >= 0xe0:
				probe.utf8 = 2
			case value >= 0xc0:
				probe.utf8 = 1
			case value >= 0x80 && probe.utf8 != 0:
				probe.utf8--
			default:
				probe.utf8 = 0
			}
		}
	}
}

func (probe *sizeProbe) boundary() bool { return probe.state == outputGround && probe.utf8 == 0 }

// parseReport reads ESC [ rows ; columns R from what is held. complete says
// the bytes are a whole report and partial that they may still become one.
// Row 1 is excluded: it is Shift-F3 on xterm, and a usable screen's corner is
// never there.
func parseReport(held []byte) (rows, columns int, complete, partial bool) {
	if len(held) == 0 || held[0] != 0x1b {
		return 0, 0, false, false
	}
	if len(held) == 1 {
		return 0, 0, false, true
	}
	if held[1] != '[' {
		return 0, 0, false, false
	}
	semicolon := false
	for _, value := range held[2:] {
		switch {
		case value >= '0' && value <= '9':
			if semicolon {
				columns = columns*10 + int(value-'0')
			} else {
				rows = rows*10 + int(value-'0')
			}
			if rows > MaxRemoteDimension || columns > MaxRemoteDimension {
				return 0, 0, false, false
			}
		case value == ';' && !semicolon && rows != 0:
			semicolon = true
		case value == 'R' && semicolon && rows > 1 && columns > 0:
			return rows, columns, true, false
		default:
			return 0, 0, false, false
		}
	}
	return 0, 0, false, true
}

// ProbeActive reports whether the frontend should call ProbeTick.
func (engine *Engine) ProbeActive() bool {
	return !engine.failed && (engine.probe.awaiting || engine.probeWanted())
}

func (engine *Engine) probeWanted() bool {
	// A program that has written no escape sequence draws no screen, and its
	// plain output stays free of questions. One that has is past its startup
	// and ready for SIGWINCH.
	return engine.transport && engine.escaped && !engine.geometry.Authoritative && !engine.geometry.Display &&
		engine.probe.unanswered < probeUnanswered
}

// ProbeTick runs half a probe period: one tick asks, the next ends the wait
// for an answer that did not come.
func (engine *Engine) ProbeTick() {
	probe := &engine.probe
	if probe.awaiting {
		probe.awaiting = false
		probe.unanswered++
		engine.releaseHeld()
		engine.FlushInput()
	}
	if probe.resting {
		probe.resting = false
		return
	}
	if engine.probeWanted() && probe.boundary() {
		engine.emit([]byte(probeSequence))
		probe.awaiting, probe.resting = true, true
	}
}

// feedProbe takes a byte that may belong to the answer and reports whether it
// consumed it.
func (engine *Engine) feedProbe(value byte) bool {
	probe := &engine.probe
	if !probe.awaiting {
		return false
	}
	probe.held[probe.heldSize] = value
	probe.heldSize++
	rows, columns, complete, partial := parseReport(probe.held[:probe.heldSize])
	if complete {
		probe.awaiting, probe.unanswered, probe.heldSize = false, 0, 0
		if rows != engine.geometry.Rows || columns != engine.geometry.Columns {
			engine.SetGeometry(Geometry{Columns: columns, Rows: rows, Term: engine.geometry.Term})
		}
		return true
	}
	if partial && probe.heldSize < len(probe.held) {
		return true
	}
	// Not an answer: the bytes are input, and the wait goes on for the answer
	// behind them, which an ESC here may start.
	if value == 0x1b {
		probe.heldSize--
		engine.releaseHeld()
		probe.held[0], probe.heldSize = value, 1
		return true
	}
	engine.releaseHeld()
	return true
}

// releaseHeld passes held bytes on as typed.
func (engine *Engine) releaseHeld() {
	probe := &engine.probe
	size := probe.heldSize
	probe.heldSize = 0
	for index := 0; index < size; index++ {
		engine.feedTyped(probe.held[index])
	}
}
