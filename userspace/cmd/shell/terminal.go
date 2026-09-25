//go:build goos_userspace

package main

// Terminal interaction: the prompt, lines from the terminal service, a
// replaced terminal session, and output back through the terminal (ADR 0082,
// 0089).

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/config"
	"goos/userspace/runtime/sys"
	terminal "goos/userspace/tty"
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
	reading = 0
}

// installTerminal takes a new session from PID 1: the terminal service was
// restarted, and this is its client end.
func installTerminal(session handle.ID) {
	if ttyHandle != 0 {
		sys.HandleClose(ttyHandle)
	}
	ttyHandle, reading = session, 0
	mark("SHELL_TERMINAL_REPLACED_OK\n")
}

func sendTerminal(message ipc.Message) bool {
	// What the shell printed goes out before it asks for the next line.
	flushOutput()
	size, ok := ipc.Encode(message, ttyWire[:])
	return ok && sys.IPCSend(ttyHandle, ttyWire[:size]) == 0
}

// takeLine returns the answer to the outstanding read if it has arrived. The
// terminal service's other replies, acknowledgements of redraws, are consumed.
// The returned text aliases a buffer the next call reuses.
func takeLine() (terminal.Result, []byte, bool) {
	for ttyHandle != 0 {
		status := sys.IPCReceive(ttyHandle, ttyWire[:])
		if status == ipc.StatusNotFound {
			return 0, nil, false
		}
		if status != 0 {
			terminalLost()
			return 0, nil, false
		}
		payloadSize := int(ttyWire[24]) | int(ttyWire[25])<<8
		size := ipc.WireHeaderSize + payloadSize + int(ttyWire[26])*8
		if size > len(ttyWire) {
			continue
		}
		message, valid := ipc.Decode(ttyWire[:size])
		if !valid || reading == 0 || message.Correlation != reading {
			continue
		}
		result, text, valid := terminal.DecodeReply(&message)
		if !valid {
			continue
		}
		reading = 0
		size = copy(typedStorage[:], text)
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
	writeStream(outputHandle, pending, outputDeadline)
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
