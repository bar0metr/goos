//go:build goos_userspace

// The local frontend connects a terminal Engine to its console, output stream,
// and shell session. Each console has its own process and Engine.
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
	consoleHandle  handle.ID
	outputHandle   handle.ID
	sessionHandle  handle.ID
	controlHandle  handle.ID
	wire           [ipc.WireMaxSize]byte
	engine         *tty.Engine
	mirrorHandle   handle.ID
	mirroring      bool
	sessionClosed  bool
	outputClosed   bool
	outputReported bool
	rawReported    bool
	lineDelivered  bool
	outputBuffer   [kcall.MaxStreamIO]byte
)

// Bound each output pass so streaming output cannot starve console interrupts.
const outputBatch = 16

//export userspaceMain
func userspaceMain() {
	arena.InitService()
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
	engine = tty.NewEngine(writeBytes, send)
	info, status := sys.ConsoleInformation(consoleHandle)
	if status == kcall.StatusOK && info.Columns != 0 && info.Rows != 0 {
		display := info.Flags&kcall.ConsoleFlagFramebuffer != 0
		engine.SetGeometry(tty.Geometry{Columns: int(info.Columns), Rows: int(info.Rows), Display: display, Authoritative: display})
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
	mirrorHandle, _ = data.Handle("mirror")
	mark("TTY_TERMINAL_OK\n")
	for {
		serveConsole()
		serveOutput()
		serveControl()
		serveSession()
		if engine.Failed() {
			// PID 1 renews sessions after process exits, not after an endpoint
			// is merely closed. Restart rather than strand a live shell.
			fatal("TTY_SESSION_IO_FAILED\n")
		}
		awaitWork()
	}
}

// PID 1 supplies a replacement session when the shell restarts. Typed-ahead
// line input is retained; raw mode and queued events belong to the old shell.
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
			for index := 0; index < int(message.HandleCount); index++ {
				sys.HandleClose(message.Handles[index])
			}
			continue
		}
		if sessionHandle != 0 {
			sys.HandleClose(sessionHandle)
		}
		sessionHandle, sessionClosed = session, false
		wasRaw := engine.Raw()
		engine.ReplaceSession()
		reportRawReset(wasRaw)
		mark("TTY_SESSION_REPLACED_OK\n")
	}
}

func serveConsole() {
	if sys.ConsoleInterrupt(consoleHandle) == kcall.StatusOK {
		mirroring = mirrorHandle != 0
		engine.Interrupt()
	}
	for {
		input, status := sys.ConsoleRead(consoleHandle)
		if status != kcall.StatusOK {
			break
		}
		mirroring = mirrorHandle != 0
		engine.Feed(input)
	}
	engine.FlushInput()
	reportLineDelivery()
}

func serveOutput() {
	relayed := false
	for batch := 0; batch < outputBatch && !outputClosed; batch++ {
		count, status := sys.StreamRead(outputHandle, outputBuffer[:])
		if status == kcall.StatusResourceExhausted {
			break
		}
		if status != kcall.StatusOK || count == 0 || count > len(outputBuffer) {
			outputClosed = true
			syslog.Warn("output stream closed")
			break
		}
		engine.Output(outputBuffer[:count])
		relayed = true
	}
	if relayed && !outputReported && !engine.Raw() {
		outputReported = true
		mark("TTY_OUTPUT_OK\n")
	}
}

func serveSession() {
	for !sessionClosed && !engine.Failed() {
		status := sys.IPCReceive(sessionHandle, wire[:])
		if status == ipc.StatusNotFound {
			return
		}
		if status != 0 {
			closeSession()
			syslog.Warn("session closed")
			return
		}
		message, valid := decodeWire()
		if !valid {
			continue
		}
		// Output written before a request precedes its prompt.
		serveOutput()
		wasRaw := engine.Raw()
		engine.Request(&message)
		reportLineDelivery()
		reportRawReset(wasRaw)
	}
}

func reportLineDelivery() {
	if lineDelivered {
		lineDelivered = false
		mark("TTY_LINE_OK\n")
	}
}

func reportRawReset(wasRaw bool) {
	if wasRaw && !engine.Raw() && !rawReported {
		rawReported = true
		mark("TTY_RAW_MODE_OK\n")
	}
}

func closeSession() {
	sessionClosed = true
	wasRaw := engine.Raw()
	engine.CloseSession()
	reportRawReset(wasRaw)
	if sessionHandle != 0 {
		sys.HandleClose(sessionHandle)
		sessionHandle = 0
	}
}

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
		userwait.Sleep(1)
	}
}

func send(message ipc.Message) bool {
	size, ok := ipc.Encode(message, wire[:])
	delivered := ok && sys.IPCSend(sessionHandle, wire[:size]) == 0
	if delivered && message.Payload[0] == byte(tty.ResultLine) {
		lineDelivered = true
	}
	return delivered
}

func decodeWire() (ipc.Message, bool) {
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	if size > len(wire) {
		return ipc.Message{}, false
	}
	return ipc.Decode(wire[:size])
}

// Validation markers use the same output/redraw path, never a raw display.
func mark(message string) {
	if validation.Enabled && engine != nil && !engine.Raw() {
		engine.Output(sys.Bytes(message))
	}
}

func write(message string) { writeBytes(sys.Bytes(message)) }

func writeBytes(value []byte) bool {
	if len(value) == 0 {
		return true
	}
	if sys.ConsoleWrite(consoleHandle, value) != kcall.StatusOK {
		return false
	}
	if mirroring {
		sys.ConsoleWrite(mirrorHandle, value)
	}
	return true
}

func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	sys.Exit(1)
	for {
	}
}

func main() {}
