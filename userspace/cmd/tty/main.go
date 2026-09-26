//go:build goos_userspace

// The terminal service owns the console (ADR 0082, ADR 0089). It is the only
// process holding the right to read the console: it edits the line being
// typed, echoes it, and turns Ctrl-C and Ctrl-D into events, and a client gets
// finished lines by asking for them over its session.
//
// Output passes through it too. The shell and every command write to a shared
// stream whose reading end only this service holds; it writes what arrives to
// the console, taking the line being typed off the screen first and putting it
// back after, so output never lands in the middle of what someone is typing.
package main

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/tty"
	userwait "goos/userspace/wait"
)

var (
	consoleHandle handle.ID
	outputHandle  handle.ID
	sessionHandle handle.ID
	// controlHandle is the channel PID 1 hands a new session over when the
	// shell has been restarted (ADR 0089).
	controlHandle handle.ID
	wire          [ipc.WireMaxSize]byte
	line          tty.Discipline
	screen        tty.Screen
	// reading is the correlation of the client's outstanding read, or zero.
	reading uint64
	// sent is how much of the completed line earlier parts delivered.
	sent       int
	prompt     [tty.MaxPrompt]byte
	promptSize int
	// An interrupt or end of input typed while no read was outstanding is
	// kept for the next one, so a Ctrl-C between two reads is not lost.
	interruptQueued bool
	endQueued       bool
	sessionClosed   bool
	outputClosed    bool
	outputReported  bool
	outputBuffer    [kcall.MaxStreamIO]byte
	newline         = [1]byte{'\n'}
)

// outputBatch bounds the reads one pass relays, so a program writing without
// pause cannot keep the service from reading the keyboard: Ctrl-C has to get
// through while output is streaming.
const outputBatch = 16

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("TTY_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
	if !ok {
		fatal("TTY_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "tty")
	if !ok {
		fatal("TTY_CONSOLE_MISSING\n")
	}
	outputHandle, ok = data.Handle("output")
	if !ok {
		fatal("TTY_OUTPUT_MISSING\n")
	}
	sessionHandle, ok = data.Handle("session")
	if !ok {
		fatal("TTY_SESSION_MISSING\n")
	}
	controlHandle, ok = data.Handle("control")
	if !ok {
		fatal("TTY_CONTROL_MISSING\n")
	}
	mark("TTY_TERMINAL_OK\n")
	base := arena.Current()
	for {
		arena.Reset(base)
		serveConsole()
		serveOutput()
		serveControl()
		serveSession()
		awaitWork()
	}
}

// serveControl takes a new session from PID 1. The shell was restarted: the
// old session died with it, and this is the server end of its replacement's.
// What has been typed of the current line is kept for the new shell.
func serveControl() {
	for controlHandle != 0 {
		status := sys.IPCReceive(controlHandle, wire[:])
		if status == ipc.StatusNotFound {
			return
		}
		if status != 0 {
			sys.HandleClose(controlHandle)
			controlHandle = 0
			return
		}
		message, valid := decodeWire()
		if !valid {
			continue
		}
		session, valid := tty.DecodeSessionNotice(&message)
		if !valid {
			// Only PID 1 holds the other end, and it sends nothing else;
			// any handle a malformed message carried is not kept.
			for index := 0; index < int(message.HandleCount); index++ {
				sys.HandleClose(message.Handles[index])
			}
			continue
		}
		if sessionHandle != 0 {
			sys.HandleClose(sessionHandle)
		}
		sessionHandle, sessionClosed = session, false
		reading, sent, interruptQueued, endQueued = 0, 0, false, false
		mark("TTY_SESSION_REPLACED_OK\n")
	}
}

// serveConsole applies everything the console has: the interrupt first, which
// the kernel keeps out of the input ring, then each byte.
func serveConsole() {
	if sys.ConsoleInterrupt(consoleHandle) == kcall.StatusOK {
		apply(line.Feed(0x03))
	}
	for {
		input, status := sys.ConsoleRead(consoleHandle)
		if status != kcall.StatusOK {
			break
		}
		apply(line.Feed(input))
	}
}

// serveOutput relays what the shell and its commands have written. The line
// being typed is taken off the screen before the first write and put back
// after the last, so a background job's report or a command's output never
// lands inside it.
func serveOutput() {
	relayed := false
	for batch := 0; batch < outputBatch && !outputClosed; batch++ {
		count, status := sys.StreamRead(outputHandle, outputBuffer[:])
		if status == kcall.StatusResourceExhausted {
			break
		}
		if status != kcall.StatusOK || count == 0 || count > len(outputBuffer) {
			// PID 1 keeps a writing end for as long as it runs, so this is
			// not an ordinary end of output; stop waiting on the stream
			// rather than spin on it.
			outputClosed = true
			syslog.Warn("output stream closed")
			break
		}
		if !relayed {
			writeBytes(screen.Hide())
			relayed = true
		}
		data := outputBuffer[:count]
		writeBytes(data)
		screen.Output(data)
	}
	if relayed {
		showLine()
		if !outputReported {
			outputReported = true
			mark("TTY_OUTPUT_OK\n")
		}
	}
}

func apply(event tty.Event) {
	echo := line.Echo()
	writeBytes(echo)
	screen.Echoed(echo, len(line.Pending()))
	switch event {
	case tty.EventLine:
		deliverLine()
	case tty.EventInterrupt:
		sent = 0
		if !answer(tty.ResultInterrupt) {
			interruptQueued, endQueued = true, false
		}
	case tty.EventEndOfInput:
		if !answer(tty.ResultEndOfInput) {
			endQueued = true
		}
	case tty.EventRedraw:
		write("\x1b[2J\x1b[H")
		screen.Cleared()
		showLine()
	}
}

// deliverLine hands a completed line to the outstanding read. Without one the
// line waits in the discipline, which accepts no more input until it is read.
//
// A line longer than one message goes in parts: each but the last answers a
// read with ResultPart, and the client asks for the next with a continue.
func deliverLine() {
	text, complete := line.Line()
	if !complete || reading == 0 {
		return
	}
	if rest := text[sent:]; len(rest) > tty.MaxPart {
		reply(tty.ResultPart, rest[:tty.MaxPart])
		sent += tty.MaxPart
		reading = 0
		return
	}
	delivered := reply(tty.ResultLine, text[sent:])
	reading, sent = 0, 0
	line.Next()
	if delivered {
		mark("TTY_LINE_OK\n")
	}
}

// continueRead answers a request for the next part of a line. A line taken
// back meanwhile — Ctrl-C discards it — ends the read with the interrupt.
func continueRead(correlation uint64) {
	if reading != 0 {
		if refusal, ok := tty.Reply(tty.ResultRefused, nil, correlation); ok {
			send(refusal)
		}
		return
	}
	reading = correlation
	if text, complete := line.Line(); complete && sent != 0 && sent < len(text) {
		deliverLine()
		return
	}
	if interruptQueued {
		interruptQueued = false
		answer(tty.ResultInterrupt)
		return
	}
	answer(tty.ResultRefused)
}

// answer ends the outstanding read with result, reporting whether there was one.
func answer(result tty.Result) bool {
	if reading == 0 {
		return false
	}
	reply(result, nil)
	reading = 0
	return true
}

func reply(result tty.Result, text []byte) bool {
	message, ok := tty.Reply(result, text, reading)
	return ok && send(message)
}

func serveSession() {
	for !sessionClosed {
		status := sys.IPCReceive(sessionHandle, wire[:])
		if status == ipc.StatusNotFound {
			return
		}
		if status != 0 {
			// The client is gone. Input still arrives and is still edited, but
			// there is no one to give it to until the session is replaced.
			closeSession()
			syslog.Warn("session closed")
			return
		}
		message, valid := decodeWire()
		if !valid {
			continue
		}
		operation, text, valid := tty.DecodeRequest(&message)
		if !valid {
			if refusal, ok := tty.Reply(tty.ResultRefused, nil, message.Correlation); ok && message.Kind == ipc.KindRequest {
				send(refusal)
			}
			continue
		}
		// Output the client wrote before asking goes out before the prompt.
		serveOutput()
		switch operation {
		case tty.OperationReadLine:
			startRead(text, message.Correlation)
		case tty.OperationContinue:
			continueRead(message.Correlation)
		case tty.OperationRedraw:
			promptSize = copy(prompt[:], text)
			if reading != 0 {
				showLine()
			}
			if accepted, ok := tty.Reply(tty.ResultAccepted, nil, message.Correlation); ok {
				send(accepted)
			}
		}
	}
}

// closeSession forgets a session whose client has gone, closing its endpoint
// so the channel is released rather than left half open.
func closeSession() {
	sessionClosed = true
	reading, sent = 0, 0
	if sessionHandle != 0 {
		sys.HandleClose(sessionHandle)
		sessionHandle = 0
	}
}

// startRead begins a read, answering at once with anything already waiting:
// an interrupt, end of input, or a line typed ahead of the request.
func startRead(text []byte, correlation uint64) {
	if reading != 0 {
		if refusal, ok := tty.Reply(tty.ResultRefused, nil, correlation); ok {
			send(refusal)
		}
		return
	}
	reading = correlation
	promptSize = copy(prompt[:], text)
	switch {
	case interruptQueued:
		interruptQueued = false
		answer(tty.ResultInterrupt)
	case endQueued:
		endQueued = false
		answer(tty.ResultEndOfInput)
	default:
		if text, complete := line.Line(); complete {
			// A line finished before this read was echoed with no prompt,
			// possibly in the middle of other output. It is shown again with
			// the prompt it answers, as a shell's line editor does with typed
			// ahead input, so the screen always reads as a prompt followed by
			// the command it ran.
			if promptSize != 0 {
				writeBytes(screen.Hide())
				writeBytes(screen.Show(prompt[:promptSize], text))
				writeBytes(newline[:])
				screen.Echoed(newline[:], 0)
			}
			deliverLine()
			return
		}
		showLine()
	}
}

// showLine replaces whatever line is on the screen with the prompt of the
// outstanding read, if there is one, and what has been typed so far.
func showLine() {
	writeBytes(screen.Hide())
	shown := prompt[:0]
	if reading != 0 {
		shown = prompt[:promptSize]
	}
	writeBytes(screen.Show(shown, line.Pending()))
}

// awaitWork blocks until a key arrives, output is written, or the client
// sends a request.
func awaitWork() {
	var interests [userwait.MaxObjects]userwait.Interest
	interests[0] = userwait.Interest{Handle: consoleHandle, Events: kcall.WaitReadable}
	count := 1
	if !outputClosed {
		interests[count] = userwait.Interest{Handle: outputHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count++
	}
	if !sessionClosed {
		interests[count] = userwait.Interest{Handle: sessionHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count++
	}
	if controlHandle != 0 {
		interests[count] = userwait.Interest{Handle: controlHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count++
	}
	if _, status := userwait.Any(&interests, count, kcall.WaitAnyInfiniteTimeout); status != kcall.StatusOK {
		// A wait that cannot be established must not become a spin.
		userwait.Sleep(1)
	}
}

func send(message ipc.Message) bool {
	size, ok := ipc.Encode(message, wire[:])
	return ok && sys.IPCSend(sessionHandle, wire[:size]) == 0
}

func decodeWire() (ipc.Message, bool) {
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	if size > len(wire) {
		return ipc.Message{}, false
	}
	return ipc.Decode(wire[:size])
}

// mark writes a validation marker on a row of its own: it goes around the line
// being typed the way relayed output does.
func mark(message string) {
	if validation.Enabled {
		writeBytes(screen.Hide())
		write(message)
		screen.Output(newline[:])
		showLine()
	}
}

func write(message string) {
	sys.ConsoleWriteString(consoleHandle, message)
}

func writeBytes(value []byte) {
	if len(value) != 0 {
		sys.ConsoleWrite(consoleHandle, value[:])
	}
}

// fatal reports and exits, so the supervisor starts a replacement rather than
// leaving a service that neither works nor ends.
func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	sys.Exit(1)
	for {
	}
}

func main() {}
