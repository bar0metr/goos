package tty

import "goos/kernel/ipc"

// Engine owns one terminal session. Callbacks consume their arguments before
// returning; the buffers are reused. A rejected write or reply makes Failed
// sticky, so a frontend can close a blocked or disconnected session.
// Calls must be serialized by the frontend.
type Engine struct {
	line                Discipline
	screen              Screen
	raw                 bool
	rawInput            RawInput
	rawBuffer           [MaxPart]byte
	reading             uint64
	sent                int
	prompt              [MaxPrompt]byte
	promptSize          int
	interruptQueued     bool
	endQueued           bool
	geometry            Geometry
	geometrySubscribed  bool
	geometryPending     bool
	geometryOutstanding uint64
	term                [MaxTerm]byte
	write               func([]byte) bool
	send                func(ipc.Message) bool
	failed              bool
}

func NewEngine(write func([]byte) bool, send func(ipc.Message) bool) *Engine {
	engine := &Engine{write: write, send: send}
	engine.SetGeometry(Geometry{Columns: DefaultColumns, Rows: DefaultRows})
	engine.geometry.Sequence = 0
	return engine
}

func (engine *Engine) Failed() bool { return engine.failed }
func (engine *Engine) Raw() bool    { return engine.raw }

// CurrentGeometry borrows the copied TERM until the next engine update.
func (engine *Engine) CurrentGeometry() Geometry { return engine.geometry }

// SetGeometry installs trusted frontend geometry. Term is copied, not retained.
// Display and Authoritative are independent: remote PTYs are authoritative
// without being fixed framebuffer displays.
func (engine *Engine) SetGeometry(geometry Geometry) bool {
	if !geometry.Valid() || engine.failed {
		return false
	}
	engine.emit(engine.screen.Hide())
	if engine.geometry.Sequence == ^uint64(0) {
		return false
	}
	geometry.Sequence = engine.geometry.Sequence + 1
	engine.geometry = geometry
	engine.geometry.Term = engine.term[:copy(engine.term[:], geometry.Term)]
	engine.screen.SetGeometry(geometry.Columns, geometry.Rows)
	if !engine.raw {
		engine.showLine()
	}
	if engine.geometrySubscribed {
		engine.geometryPending = true
	}
	return !engine.failed
}

// SetRemoteGeometry accepts the dimensions and TERM supplied by a PTY peer.
func (engine *Engine) SetRemoteGeometry(columns, rows int, term []byte) bool {
	geometry := Geometry{Columns: columns, Rows: rows, Authoritative: true, Term: term}
	return geometry.ValidRemote() && engine.SetGeometry(geometry)
}

// FlushGeometry attempts one nonblocking notification. Backpressure retains
// only the newest snapshot and does not fail or delay terminal input.
func (engine *Engine) FlushGeometry() {
	if !engine.geometryPending || engine.geometryOutstanding != 0 || engine.failed {
		return
	}
	if notice, ok := GeometryNotice(engine.geometry); ok && engine.send != nil && engine.send(notice) {
		engine.geometryPending = false
		engine.geometryOutstanding = engine.geometry.Sequence
	}
}

func (engine *Engine) GeometryPending() bool {
	return engine.geometryPending && engine.geometryOutstanding == 0
}

// Feed accepts transport bytes. Ctrl-C takes the same path as an out-of-band
// console interrupt, including in raw mode.
func (engine *Engine) Feed(value byte) {
	if engine.failed {
		return
	}
	if value == keyInterrupt {
		engine.Interrupt()
		return
	}
	if engine.raw {
		engine.rawInput.Feed(value)
		return
	}
	engine.apply(engine.line.Feed(value))
}

// FlushInput answers an outstanding raw read after a bounded input batch.
func (engine *Engine) FlushInput() { engine.deliverRaw() }

// Interrupt is the local console's out-of-band Ctrl-C entry point.
func (engine *Engine) Interrupt() {
	if engine.failed {
		return
	}
	if !engine.raw {
		engine.apply(engine.line.Feed(keyInterrupt))
		return
	}
	engine.sent = 0
	// Bytes preceding an interrupt must not become input to the next job.
	engine.rawInput.Reset()
	if !engine.answer(ResultInterrupt) {
		engine.interruptQueued, engine.endQueued = true, false
	}
}

// Output relays one bounded batch of program output around the edited line.
func (engine *Engine) Output(data []byte) {
	if engine.failed || len(data) == 0 {
		return
	}
	if engine.raw {
		engine.emit(data)
		return
	}
	engine.emit(engine.screen.Hide())
	engine.emit(data)
	engine.screen.Output(data)
	engine.showLine()
}

// Request validates and executes one client operation. Malformed messages
// receive a refusal only if they are requests with a usable correlation.
func (engine *Engine) Request(message *ipc.Message) {
	if engine.failed {
		return
	}
	if sequence, valid := DecodeGeometryAcknowledgement(message); valid {
		if sequence == engine.geometryOutstanding {
			engine.geometryOutstanding = 0
		}
		return
	}
	operation, text, valid := DecodeRequest(message)
	if !valid {
		if message.Kind == ipc.KindRequest && message.Correlation != 0 {
			engine.respond(ResultRefused, nil, message.Correlation)
		}
		return
	}
	switch operation {
	case OperationReadLine:
		engine.startRead(text, message.Correlation)
	case OperationContinue:
		engine.continueRead(message.Correlation)
	case OperationRedraw:
		engine.promptSize = copy(engine.prompt[:], text)
		if engine.reading != 0 && !engine.raw {
			engine.showLine()
		}
		engine.respond(ResultAccepted, nil, message.Correlation)
	case OperationRawMode:
		engine.enterRaw()
		engine.respond(ResultAccepted, nil, message.Correlation)
		engine.deliverRaw()
	case OperationLineMode:
		engine.leaveRaw()
		engine.respond(ResultAccepted, nil, message.Correlation)
	case OperationGeometry:
		engine.geometrySubscribed = true
		engine.geometryPending = false
		reply, ok := GeometryStateReply(engine.geometry, message.Correlation)
		if ok {
			engine.sendMessage(reply)
		}
	}
}

// CloseSession abandons outstanding reads and raw input, but keeps the edited
// line so a local shell restart does not discard typed-ahead input.
func (engine *Engine) CloseSession() {
	engine.reading, engine.sent = 0, 0
	engine.geometrySubscribed, engine.geometryPending, engine.geometryOutstanding = false, false, 0
	engine.leaveRaw()
}

// ReplaceSession clears events owned by the old client and restores line mode.
func (engine *Engine) ReplaceSession() {
	failed := engine.failed
	engine.failed = false
	engine.interruptQueued, engine.endQueued = false, false
	engine.CloseSession()
	if failed {
		engine.emit([]byte(ResetSequence))
		engine.screen.Cleared()
	}
}

func (engine *Engine) apply(event Event) {
	echo := engine.line.Echo()
	engine.emit(echo)
	engine.screen.Echoed(echo, len(engine.line.Pending()))
	switch event {
	case EventLine:
		engine.deliverLine()
	case EventInterrupt:
		engine.sent = 0
		if !engine.answer(ResultInterrupt) {
			engine.interruptQueued, engine.endQueued = true, false
		}
	case EventEndOfInput:
		if !engine.answer(ResultEndOfInput) {
			engine.endQueued = true
		}
	case EventRedraw:
		engine.emit([]byte("\x1b[2J\x1b[H"))
		engine.screen.Cleared()
		engine.showLine()
	}
}

func (engine *Engine) deliverRaw() {
	if engine.failed || !engine.raw || engine.reading == 0 || engine.rawInput.Len() == 0 {
		return
	}
	count := engine.rawInput.Take(engine.rawBuffer[:])
	engine.respond(ResultRaw, engine.rawBuffer[:count], engine.reading)
	engine.reading = 0
}

func (engine *Engine) enterRaw() {
	if engine.raw {
		return
	}
	engine.emit(engine.screen.Hide())
	engine.raw = true
	engine.rawInput.Reset()
}

func (engine *Engine) leaveRaw() {
	if !engine.raw {
		return
	}
	engine.raw = false
	engine.rawInput.Reset()
	engine.emit([]byte(ResetSequence))
	engine.screen.Cleared()
	if engine.reading != 0 {
		engine.showLine()
	}
}

func (engine *Engine) deliverLine() {
	text, complete := engine.line.Line()
	if !complete || engine.reading == 0 {
		return
	}
	if rest := text[engine.sent:]; len(rest) > MaxPart {
		engine.respond(ResultPart, rest[:MaxPart], engine.reading)
		engine.sent += MaxPart
		engine.reading = 0
		return
	}
	engine.respond(ResultLine, text[engine.sent:], engine.reading)
	engine.reading, engine.sent = 0, 0
	engine.line.Next()
}

func (engine *Engine) continueRead(correlation uint64) {
	if engine.reading != 0 {
		engine.respond(ResultRefused, nil, correlation)
		return
	}
	engine.reading = correlation
	if text, complete := engine.line.Line(); complete && engine.sent != 0 && engine.sent < len(text) {
		engine.deliverLine()
		return
	}
	if engine.interruptQueued {
		engine.interruptQueued = false
		engine.answer(ResultInterrupt)
		return
	}
	engine.answer(ResultRefused)
}

func (engine *Engine) answer(result Result) bool {
	if engine.reading == 0 {
		return false
	}
	engine.respond(result, nil, engine.reading)
	engine.reading = 0
	return true
}

func (engine *Engine) respond(result Result, text []byte, correlation uint64) {
	if reply, ok := Reply(result, text, correlation); ok {
		engine.sendMessage(reply)
	}
}

func (engine *Engine) startRead(text []byte, correlation uint64) {
	if engine.reading != 0 {
		engine.respond(ResultRefused, nil, correlation)
		return
	}
	engine.reading = correlation
	engine.promptSize = copy(engine.prompt[:], text)
	if engine.interruptQueued {
		engine.interruptQueued = false
		engine.answer(ResultInterrupt)
		return
	}
	if engine.raw {
		engine.deliverRaw()
		return
	}
	if engine.endQueued {
		engine.endQueued = false
		engine.answer(ResultEndOfInput)
		return
	}
	if text, complete := engine.line.Line(); complete {
		if engine.promptSize != 0 {
			engine.emit(engine.screen.Hide())
			engine.emit(engine.screen.Show(engine.prompt[:engine.promptSize], text))
			engine.emit([]byte{'\n'})
			engine.screen.Echoed([]byte{'\n'}, 0)
		}
		engine.deliverLine()
		return
	}
	engine.showLine()
}

func (engine *Engine) showLine() {
	engine.emit(engine.screen.Hide())
	shown := engine.prompt[:0]
	if engine.reading != 0 {
		shown = engine.prompt[:engine.promptSize]
	}
	engine.emit(engine.screen.Show(shown, engine.line.Pending()))
}

func (engine *Engine) emit(data []byte) {
	for len(data) != 0 && !engine.failed {
		count := len(data)
		if count > MaxPart {
			count = MaxPart
		}
		if engine.write == nil || !engine.write(data[:count]) {
			engine.failed = true
			return
		}
		data = data[count:]
	}
}

func (engine *Engine) sendMessage(message ipc.Message) {
	if !engine.failed && (engine.send == nil || !engine.send(message)) {
		engine.failed = true
	}
}
