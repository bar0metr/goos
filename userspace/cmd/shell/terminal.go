//go:build goos_userspace

package main

// Terminal interaction: the prompt, lines from the terminal service, a
// replaced terminal session, and output back through the terminal (ADR 0082,
// 0089).

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/config"
	execproto "goos/userspace/exec"
	"goos/userspace/runtime/sys"
	shellcmd "goos/userspace/shell"
	terminal "goos/userspace/tty"
	userwait "goos/userspace/wait"
)

// showPrompt asks for the next line with the shell's prompt. A read left
// outstanding by a foreground command already holds what was typed ahead, so
// it is shown again with the prompt instead of being asked for anew. Without a
// terminal there is no one to ask until PID 1 hands over a new session.
func showPrompt() {
	if ttyHandle == 0 {
		return
	}
	if reading != 0 {
		redraw(promptText[:promptSize])
		return
	}
	requestLine(promptText[:promptSize])
}

func requestLine(prompt []byte) {
	if ttyHandle == 0 {
		return
	}
	correlation++
	request, ok := terminal.Request(terminal.OperationReadLine, prompt, correlation)
	if !ok {
		fatal("SHELL_TERMINAL_REQUEST_FAILED\n")
	}
	if !sendTerminal(request) {
		terminalLost()
		return
	}
	reading = correlation
}

// Authoritative dimensions and TERM reach children without granting console
// access. A remote PTY is authoritative but is not a fixed display. Serial
// fallback dimensions remain non-authoritative, so programs may probe them.
var (
	childTerminalEnvironment shellcmd.TerminalEnvironment
	geometryRequest          uint64
	queuedTerminal           ipc.Message
	queuedTerminalValid      bool
)

// requestGeometry asks the terminal service for its size; the answer is taken
// with the other replies in takeLine.
func requestGeometry() {
	if ttyHandle == 0 {
		return
	}
	correlation++
	request, ok := terminal.Request(terminal.OperationGeometry, nil, correlation)
	if !ok {
		return
	}
	if !sendTerminal(request) {
		terminalLost()
		return
	}
	geometryRequest = correlation
}

func storeGeometry(payload []byte) { childTerminalEnvironment.Store(payload) }

// refreshGeometry takes a new snapshot before building a job's environment.
// An outstanding read's reply is kept intact for takeLine; refreshing geometry
// must not consume typed-ahead input between commands in a sequence.
func refreshGeometry() bool {
	if ttyHandle == 0 {
		return false
	}
	requestGeometry()
	if ttyHandle == 0 {
		return false
	}
	start, status := sys.ClockRead()
	if status != kcall.StatusOK {
		return false
	}
	for geometryRequest != 0 && ttyHandle != 0 {
		now, clockStatus := sys.ClockRead()
		if clockStatus != kcall.StatusOK || now-start >= outputDeadline {
			return false
		}
		status := sys.IPCReceive(ttyHandle, ttyWire[:])
		if status == ipc.StatusNotFound {
			var interests [userwait.MaxObjects]userwait.Interest
			interests[0] = userwait.Interest{Handle: ttyHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			if _, waitStatus := userwait.Any(&interests, 1, outputDeadline-(now-start)); waitStatus != kcall.StatusOK {
				return false
			}
			continue
		}
		if status != 0 {
			terminalLost()
			return false
		}
		message, valid := decodeTerminalWire()
		if !valid {
			continue
		}
		result, text, valid := terminal.DecodeReply(&message)
		if !valid {
			continue
		}
		if result == terminal.ResultGeometry && message.Correlation == geometryRequest {
			storeGeometry(text)
			geometryRequest = 0
			return true
		}
		if reading != 0 && message.Correlation == reading {
			if queuedTerminalValid {
				return false
			}
			queuedTerminal, queuedTerminalValid = message, true
		}
	}
	return false
}

func decodeTerminalWire() (ipc.Message, bool) {
	payloadSize := int(ttyWire[24]) | int(ttyWire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(ttyWire[26])*8
	if size > len(ttyWire) {
		return ipc.Message{}, false
	}
	return ipc.Decode(ttyWire[:size])
}

func terminalEnvironment() [][]byte { return childTerminalEnvironment.Entries(pathVariable) }

// Raw terminal mode for a full-screen foreground command (ADR 0124). The
// shell holds the only session with the terminal service, so it is the one
// that asks: for the foreground job of a system command whose manifest
// declares GrantTerminal, once PID 1 reports the job started, and back to
// line mode as soon as the job has ended — whether it exited, crashed, or was
// stopped.
var (
	rawJob    bool
	rawActive bool
)

func setTerminalMode(operation terminal.Operation) {
	if ttyHandle == 0 {
		return
	}
	correlation++
	if request, ok := terminal.Request(operation, nil, correlation); ok && !sendTerminal(request) {
		terminalLost()
	}
}

// startRawJob switches the terminal to raw mode for the foreground job.
func startRawJob() {
	if rawJob && !rawActive {
		rawActive = true
		setTerminalMode(terminal.OperationRawMode)
	}
}

// endRawJob returns the terminal to line mode after the job, which also
// resets whatever the program left on the screen.
func endRawJob() {
	rawJob = false
	if rawActive {
		rawActive = false
		setTerminalMode(terminal.OperationLineMode)
		mark("SHELL_TERMINAL_RESTORED_OK\n")
	}
}

// wantsRawTerminal reports whether path names a command of the trusted
// index that asks for the terminal in raw mode. A program elsewhere, under
// whatever name, gets the line discipline.
func wantsRawTerminal(path []byte) bool {
	command, found := execproto.CommandFromSearchPath(path)
	if !found {
		return false
	}
	index, ok := systemCommands()
	if !ok {
		return false
	}
	entry, found := commands.Lookup(index, command)
	return found && entry.Grants&commands.GrantTerminal != 0
}

func redraw(prompt []byte) {
	if ttyHandle == 0 {
		return
	}
	correlation++
	if request, ok := terminal.Request(terminal.OperationRedraw, prompt, correlation); ok && !sendTerminal(request) {
		terminalLost()
	}
}

// terminalLost forgets a session whose terminal service has gone. The shell
// keeps running — its jobs, its output, its state — and waits for PID 1 to
// hand it the session of the terminal's replacement (ADR 0089).
func terminalLost() {
	if ttyHandle != 0 {
		sys.HandleClose(ttyHandle)
		ttyHandle = 0
	}
	reading, typedSize, geometryRequest = 0, 0, 0
	childTerminalEnvironment.Reset()
	queuedTerminalValid = false
}

// installTerminal takes a new session from PID 1: the terminal service was
// restarted, and this is its client end.
func installTerminal(session handle.ID) {
	if ttyHandle != 0 {
		sys.HandleClose(ttyHandle)
	}
	ttyHandle, reading, typedSize = session, 0, 0
	childTerminalEnvironment.Reset()
	queuedTerminalValid = false
	mark("SHELL_TERMINAL_REPLACED_OK\n")
	requestGeometry()
	// A restarted service starts in line mode; a full-screen job still
	// running needs raw mode again.
	if rawActive {
		setTerminalMode(terminal.OperationRawMode)
	}
}

// installFilesystem takes the filesystem session PID 1 hands over after the
// VFS was restarted: the old one died with it (ADR 0099). The working
// directory is kept; a later command reports it missing if it was in /tmp,
// which a restarted VFS starts empty.
func installFilesystem(session handle.ID) {
	if vfsHandle != 0 {
		sys.HandleClose(vfsHandle)
	}
	vfsHandle = session
	mark("SHELL_VFS_RENEWED_OK\n")
}

// installSettings takes the configuration session PID 1 hands over after the
// configuration service was restarted: the old one died with it (ADR 0100).
func installSettings(session handle.ID) {
	if configHandle != 0 {
		sys.HandleClose(configHandle)
	}
	configHandle = session
	mark("SHELL_CONFIG_RENEWED_OK\n")
}

func sendTerminal(message ipc.Message) bool {
	// What the shell printed goes out before it asks for the next line.
	flushOutput()
	size, ok := ipc.Encode(message, ttyWire[:])
	return ok && sys.IPCSend(ttyHandle, ttyWire[:size]) == 0
}

// takeLine returns the answer to the outstanding read if it has arrived: a
// line, raw bytes in raw mode, an interrupt or end of input. The terminal
// service's other replies, acknowledgements and the terminal's size, are
// consumed.
// The returned text aliases a buffer the next call reuses.
func takeLine() (terminal.Result, []byte, bool) {
	for ttyHandle != 0 {
		var message ipc.Message
		if queuedTerminalValid {
			message, queuedTerminalValid = queuedTerminal, false
		} else {
			status := sys.IPCReceive(ttyHandle, ttyWire[:])
			if status == ipc.StatusNotFound {
				return 0, nil, false
			}
			if status != 0 {
				terminalLost()
				return 0, nil, false
			}
			decoded, valid := decodeTerminalWire()
			if !valid {
				continue
			}
			message = decoded
		}
		result, text, valid := terminal.DecodeReply(&message)
		if valid && result == terminal.ResultGeometry && message.Correlation == geometryRequest {
			storeGeometry(text)
			geometryRequest = 0
			continue
		}
		if !valid || reading == 0 || message.Correlation != reading {
			continue
		}
		reading = 0
		if !terminal.CarriesText(result) {
			typedSize = 0
			return result, nil, true
		}
		if len(text) > len(typedStorage)-typedSize {
			// The service sends at most MaxLine bytes; more is not a line.
			typedSize = 0
			return terminal.ResultRefused, nil, true
		}
		typedSize += copy(typedStorage[typedSize:], text)
		if result == terminal.ResultPart {
			// A long line comes in parts; ask for the next one.
			correlation++
			request, ok := terminal.Request(terminal.OperationContinue, nil, correlation)
			if !ok || !sendTerminal(request) {
				typedSize = 0
				terminalLost()
				return 0, nil, false
			}
			reading = correlation
			continue
		}
		size := typedSize
		typedSize = 0
		return result, typedStorage[:size], true
	}
	return 0, nil, false
}

// loadPrompt asks the configuration service what the prompt is. A shell that
// cannot reach the service, or a value it will not accept, leaves the built-in
// text in place: configuration decides what the prompt is, not whether the
// shell starts.
func loadPrompt() {
	if configHandle == 0 {
		return
	}
	reply, ok := callConfig(config.Request{Operation: config.OperationGet, Key: []byte("system.shell.prompt")})
	if !ok || reply.Status != config.StatusOK || reply.Value.Kind != config.KindText || reply.Value.Size == 0 {
		return
	}
	// The terminal service shows only printable prompts, so a value it would
	// refuse keeps the built-in one rather than leaving the shell promptless.
	if _, valid := terminal.Request(terminal.OperationReadLine, reply.Value.Bytes(), 1); !valid {
		return
	}
	promptSize = copy(promptText[:], reply.Value.Bytes())
}

func write(message string) {
	writeBytes(sys.Bytes(message))
}

// writeBytes queues value for the terminal and sends what is queued at the end
// of a line or when the buffer fills.
func writeBytes(value []byte) {
	if len(value) == 0 {
		return
	}
	for len(value) != 0 {
		copied := copy(outputLine[outputSize:], value)
		outputSize += copied
		value = value[copied:]
		if outputSize == len(outputLine) {
			flushOutput()
		}
	}
	if outputSize != 0 && outputLine[outputSize-1] == '\n' {
		flushOutput()
	}
}

// flushOutput sends the queued output. A full stream means the service is
// behind; the shell waits for room, but not for ever: a terminal that is not
// draining must not hold the shell, so what cannot be written within the
// deadline is dropped.
func flushOutput() {
	pending := outputLine[:outputSize]
	outputSize = 0
	if !writeStream(outputHandle, pending, outputDeadline) && oneShot {
		commandStatus = shellcmd.StatusFailure
	}
}

// checkOutputIsWriteOnly proves, in the validation build, that the shell's
// terminal endpoint writes and nothing else: reading it, which would let the
// shell see what commands print to the terminal, is refused by the kernel.
func checkOutputIsWriteOnly() {
	var probe [8]byte
	if _, status := sys.StreamRead(outputHandle, probe[:]); status == kcall.StatusPermissionDenied {
		mark("TERMINAL_STREAM_READ_DENIED_OK\n")
	}
}

// checkNoLoaderAuthority proves, in the validation build, that the shell
// cannot create an executable object: that is PID 1's capability alone, and
// no handle the shell holds, nor the slot where PID 1 keeps it, is taken for
// it (ADR 0097).
func checkNoLoaderAuthority() {
	for _, candidate := range []handle.ID{0, logHandle, outputHandle, handle.ID(0x0000000100000017)} {
		if _, status := sys.ExecutableCreate(candidate, 1); status != kcall.StatusPermissionDenied {
			return
		}
	}
	mark("EXECUTABLE_CREATE_DENIED_OK\n")
}

func writeUint(value uint64) {
	var digits [20]byte
	position := len(digits)
	if value == 0 {
		write("0")
		return
	}
	for value != 0 {
		position--
		digits[position] = byte(value%10) + '0'
		value /= 10
	}
	writeBytes(digits[position:])
}
