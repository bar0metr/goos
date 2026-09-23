//go:build goos_userspace

// The terminal service owns the console's input (ADR 0082). It is the only
// process holding the right to read the console: it edits the line being typed,
// echoes it, and turns Ctrl-C and Ctrl-D into events, and a client gets
// finished lines by asking for them over its session.
//
// Output does not pass through it. Processes write to the console with a
// write-only capability, so the service redraws the prompt and the line being
// typed when a client says output has moved the cursor.
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
	sessionHandle handle.ID
	startupWire   [startup.Size]byte
	wire          [ipc.WireMaxSize]byte
	heapPointer   uintptr
	inputByte     byte
	line          tty.Discipline
	// reading is the correlation of the client's outstanding read, or zero.
	reading    uint64
	prompt     [tty.MaxPrompt]byte
	promptSize int
	// An interrupt or end of input typed while no read was outstanding is
	// kept for the next one, so a Ctrl-C between two reads is not lost.
	interruptQueued bool
	endQueued       bool
	sessionClosed   bool
)

// eraseLine returns the cursor to the start of the line and clears it.
const eraseLine = "\r\x1b[K"

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

//go:linkname startupRead goos_startup_read
func startupRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname diagnostics goos_diagnostics
func diagnostics(device uint64, address uintptr) uint64

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
	sessionHandle, ok = data.Handle("session")
	if !ok {
		fatal("TTY_SESSION_MISSING\n")
	}
	mark("TTY_TERMINAL_OK\n")
	base := heapPointer
	for {
		heapPointer = base
		serveConsole()
		serveSession()
		awaitWork()
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

func apply(event tty.Event) {
	writeBytes(line.Echo())
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
	if reply(tty.ResultLine, text) {
		mark("TTY_LINE_OK\n")
	}
	reading = 0
	line.Next()
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
			sessionClosed = true
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
		switch operation {
		case tty.OperationReadLine:
			startRead(text, message.Correlation)
		case tty.OperationRedraw:
			promptSize = copy(prompt[:], text)
			if reading != 0 {
				// Only text typed ahead is on the cursor's line; without it the
				// client's own output ended the line and there is nothing to
				// erase.
				if len(line.Pending()) != 0 {
					write(eraseLine)
				}
				showLine()
			}
			if accepted, ok := tty.Reply(tty.ResultAccepted, nil, message.Correlation); ok {
				send(accepted)
			}
		}
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

// showLine writes the prompt and what has been typed so far.
func showLine() {
	writeBytes(prompt[:promptSize])
	writeBytes(line.Pending())
}

// awaitWork blocks until a key arrives or the client sends a request.
func awaitWork() {
	var interests [userwait.MaxObjects]userwait.Interest
	interests[0] = userwait.Interest{Handle: consoleHandle, Events: kcall.WaitReadable}
	count := 1
	if !sessionClosed {
		interests[1] = userwait.Interest{Handle: sessionHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count = 2
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

func mark(message string) {
	if validation.Enabled {
		write(message)
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

func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	diagnostics(uint64(consoleHandle), 0)
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
