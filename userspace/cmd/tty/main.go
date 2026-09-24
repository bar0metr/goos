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
	"goos/userspace/startup"
	"goos/userspace/tty"
	userwait "goos/userspace/wait"
	"runtime"
	"unsafe"
)

var (
	consoleHandle handle.ID
	outputHandle  handle.ID
	sessionHandle handle.ID
	// controlHandle is the channel PID 1 hands a new session over when the
	// shell has been restarted (ADR 0089).
	controlHandle handle.ID
	startupWire   [startup.Size]byte
	wire          [ipc.WireMaxSize]byte
	heapPointer   uintptr
	inputByte     byte
	line          tty.Discipline
	screen        tty.Screen
	// reading is the correlation of the client's outstanding read, or zero.
	reading    uint64
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
	outputRequest   [3]uint64
	newline         = [1]byte{'\n'}
)

// outputBatch bounds the reads one pass relays, so a program writing without
// pause cannot keep the service from reading the keyboard: Ctrl-C has to get
// through while output is streaming.
const outputBatch = 16

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

//go:linkname consoleWrite goos_console_write
func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname consoleRead goos_console_read
func consoleRead(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname consoleInterrupt goos_console_interrupt
func consoleInterrupt(device uint64) uint64

//go:linkname ipcSend goos_ipc_send
func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcReceive goos_ipc_receive
func ipcReceive(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname streamRead goos_stream_read
func streamRead(stream uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname handleClose goos_handle_close
func handleClose(id uint64) uint64

//go:linkname startupRead goos_startup_read
func startupRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname processExit goos_exit
func processExit(code uint64)

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
		fatal("TTY_STARTUP_UNAVAILABLE\n")
	}
	data, ok := startup.Decode(startupWire[:])
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
	base := heapPointer
	for {
		heapPointer = base
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
		status := ipcReceive(uint64(controlHandle), unsafe.Pointer(&wire[0]), ipc.WireMaxSize)
		if status == uint64(ipc.StatusNotFound) {
			return
		}
		if status != 0 {
			handleClose(uint64(controlHandle))
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
				handleClose(uint64(message.Handles[index]))
			}
			continue
		}
		if sessionHandle != 0 {
			handleClose(uint64(sessionHandle))
		}
		sessionHandle, sessionClosed = session, false
		reading, interruptQueued, endQueued = 0, false, false
		mark("TTY_SESSION_REPLACED_OK\n")
	}
}

// serveConsole applies everything the console has: the interrupt first, which
// the kernel keeps out of the input ring, then each byte.
func serveConsole() {
	if consoleInterrupt(uint64(consoleHandle)) == 0 {
		apply(line.Feed(0x03))
	}
	for consoleRead(uint64(consoleHandle), unsafe.Pointer(&inputByte), 1) == 0 {
		apply(line.Feed(inputByte))
	}
}

// serveOutput relays what the shell and its commands have written. The line
// being typed is taken off the screen before the first write and put back
// after the last, so a background job's report or a command's output never
// lands inside it.
func serveOutput() {
	relayed := false
	for batch := 0; batch < outputBatch && !outputClosed; batch++ {
		outputRequest[0] = uint64(uintptr(unsafe.Pointer(&outputBuffer[0])))
		outputRequest[1], outputRequest[2] = uint64(len(outputBuffer)), 0
		status := streamRead(uint64(outputHandle), unsafe.Pointer(&outputRequest[0]), 24)
		if status == uint64(kcall.StatusResourceExhausted) {
			break
		}
		if status != 0 || outputRequest[2] == 0 || outputRequest[2] > uint64(len(outputBuffer)) {
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
		data := outputBuffer[:outputRequest[2]]
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
func deliverLine() {
	text, complete := line.Line()
	if !complete || reading == 0 {
		return
	}
	delivered := reply(tty.ResultLine, text)
	reading = 0
	line.Next()
	if delivered {
		mark("TTY_LINE_OK\n")
	}
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
		status := ipcReceive(uint64(sessionHandle), unsafe.Pointer(&wire[0]), ipc.WireMaxSize)
		if status == uint64(ipc.StatusNotFound) {
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
	reading = 0
	if sessionHandle != 0 {
		handleClose(uint64(sessionHandle))
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
		if _, complete := line.Line(); complete {
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
	return ok && ipcSend(uint64(sessionHandle), unsafe.Pointer(&wire[0]), uintptr(size)) == 0
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
	consoleWrite(uint64(consoleHandle), unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
}

func writeBytes(value []byte) {
	if len(value) != 0 {
		consoleWrite(uint64(consoleHandle), unsafe.Pointer(&value[0]), uintptr(len(value)))
	}
}

// fatal reports and exits, so the supervisor starts a replacement rather than
// leaving a service that neither works nor ends.
func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	processExit(1)
	for {
	}
}

func main() {}

//go:linkname runtimeInitHeap runtime.initHeap
func runtimeInitHeap() { heapPointer = uintptr(unsafe.Pointer(&heapStartSymbol)) }

//go:linkname runtimeAlloc runtime.alloc
func runtimeAlloc(size uintptr, _ unsafe.Pointer) unsafe.Pointer {
	size = (size + 15) &^ 15
	start, end := heapPointer, heapPointer+size
	if end < start || end > uintptr(unsafe.Pointer(&heapEndSymbol)) {
		fatal("TTY_OUT_OF_MEMORY\n")
	}
	heapPointer = end
	cleared := unsafe.Slice((*byte)(unsafe.Pointer(start)), size)
	for index := range cleared {
		cleared[index] = 0
	}
	return unsafe.Pointer(start)
}

//go:linkname runtimeFree runtime.free
func runtimeFree(unsafe.Pointer) {}

//go:linkname runtimeMarkRoots runtime.markRoots
func runtimeMarkRoots(uintptr, uintptr) {}

//go:linkname runtimeGC runtime.GC
func runtimeGC() {}

//go:linkname runtimeSetFinalizer runtime.SetFinalizer
func runtimeSetFinalizer(interface{}, interface{}) {}

//go:linkname runtimeReadMemStats runtime.ReadMemStats
func runtimeReadMemStats(*runtime.MemStats) {}
