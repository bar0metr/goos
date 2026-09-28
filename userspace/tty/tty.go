// Package tty defines the terminal service's session protocol and its line
// discipline. The terminal service is the only process that reads the console;
// a client asks it for lines and receives them edited, or an interrupt, or the
// end of input (ADR 0082). A client may also switch the session to raw mode,
// in which reads are answered with the bytes typed, unedited and unechoed, for
// a full-screen program in the foreground (ADR 0124).
package tty

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const (
	ProtocolID = uint64(0x4754545932303031)
	Version    = uint16(3)
	// MaxLine is the longest line a client receives: a native path and then
	// some (ADR 0109). A line longer than one message is delivered in parts.
	MaxLine = 4096
	// MaxPart is the most of a line one reply carries.
	MaxPart = ipc.MaxPayload - 1
	// MaxPrompt is the longest prompt a client may ask to be shown.
	MaxPrompt = 64
)

// Operation is a client request.
type Operation uint8

const (
	// OperationReadLine asks for the next line, showing the prompt. It is
	// answered when a line is complete, when Ctrl-C is typed, or at end of
	// input. One may be outstanding at a time.
	OperationReadLine Operation = iota + 1
	// OperationRedraw replaces the prompt of the outstanding read and shows it
	// again with what has been typed. Output no longer needs it: the service
	// takes the line off the screen and puts it back around every write it
	// relays (ADR 0089); a client sends it to change the prompt of a read it
	// left outstanding.
	OperationRedraw
	// OperationSession is not a request but a notification from PID 1. It
	// carries one endpoint of a new session: the server end to the terminal
	// service over its control channel, and the client end to the shell over
	// its exec channel, when the other one has been restarted (ADR 0089).
	OperationSession
	// OperationContinue asks for the next part of a line whose previous part
	// was answered with ResultPart.
	OperationContinue
	// OperationRawMode switches the session to raw mode: reads are answered
	// with ResultRaw as soon as anything is typed, nothing is echoed or
	// edited, and output is relayed without the edited line around it.
	// Ctrl-C still answers a read with ResultInterrupt. It is answered with
	// ResultAccepted.
	OperationRawMode
	// OperationLineMode returns the session to line mode and resets the
	// terminal: default colours, a visible cursor, a clear screen. It is
	// answered with ResultAccepted. A replaced or closed session, and a
	// restarted service, are in line mode.
	OperationLineMode
	// OperationGeometry asks for the terminal's size, answered with
	// ResultGeometry.
	OperationGeometry
)

// Result says how a read ended.
type Result uint8

const (
	ResultLine Result = iota + 1
	ResultInterrupt
	ResultEndOfInput
	// ResultAccepted answers a redraw.
	ResultAccepted
	// ResultRefused answers a malformed request, or a read while one is
	// already outstanding.
	ResultRefused
	// ResultPart carries part of a line; the client asks for the rest with
	// OperationContinue, and the last part comes as ResultLine.
	ResultPart
	// ResultRaw carries 1 to MaxPart bytes typed in raw mode.
	ResultRaw
	// ResultGeometry carries the terminal's columns and rows, each as a
	// little-endian uint16, and a flags byte: GeometryDisplay when they are
	// the size of a display of fixed size, the framebuffer, rather than the
	// fallback of a serial-only console, whose far end has a size of its own.
	ResultGeometry
)

// GeometrySize is the length of a ResultGeometry payload.
const GeometrySize = 5

// GeometryDisplay marks a geometry that is a fixed display's.
const GeometryDisplay = byte(1)

// Request encodes a client request carrying a prompt.
func Request(operation Operation, prompt []byte, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || !clientOperation(operation) || len(prompt) > MaxPrompt || !printable(prompt) || !takesPrompt(operation) && len(prompt) != 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(1 + len(prompt))}
	message.Payload[0] = byte(operation)
	copy(message.Payload[1:], prompt)
	return message, true
}

// DecodeRequest validates a request; the prompt aliases the message.
func DecodeRequest(message *ipc.Message) (Operation, []byte, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize == 0 || message.PayloadSize > 1+MaxPrompt {
		return 0, nil, false
	}
	operation := Operation(message.Payload[0])
	prompt := message.Payload[1:message.PayloadSize]
	if !clientOperation(operation) || !printable(prompt) || !takesPrompt(operation) && len(prompt) != 0 {
		return 0, nil, false
	}
	return operation, prompt, true
}

func clientOperation(operation Operation) bool {
	return operation >= OperationReadLine && operation <= OperationGeometry && operation != OperationSession
}

// takesPrompt reports whether a request may carry a prompt; the others carry
// nothing.
func takesPrompt(operation Operation) bool {
	return operation == OperationReadLine || operation == OperationRedraw
}

// CarriesText reports whether a result may carry input: part of a line, or
// raw bytes.
func CarriesText(result Result) bool {
	return result == ResultLine || result == ResultPart || result == ResultRaw
}

// validPayload reports whether a result may carry line: text results carry at
// most MaxPart bytes, the part and raw ones at least one, a geometry exactly
// GeometrySize, and the rest nothing.
func validPayload(result Result, line []byte) bool {
	switch {
	case result < ResultLine || result > ResultGeometry:
		return false
	case result == ResultGeometry:
		return len(line) == GeometrySize
	case !CarriesText(result):
		return len(line) == 0
	case result == ResultPart || result == ResultRaw:
		return len(line) != 0 && len(line) <= MaxPart
	}
	return len(line) <= MaxPart
}

// GeometryReply encodes the answer to OperationGeometry.
func GeometryReply(columns, rows int, display bool, correlation uint64) (ipc.Message, bool) {
	if columns <= 0 || rows <= 0 || columns > 0xffff || rows > 0xffff {
		return ipc.Message{}, false
	}
	payload := [GeometrySize]byte{byte(columns), byte(columns >> 8), byte(rows), byte(rows >> 8)}
	if display {
		payload[4] = GeometryDisplay
	}
	return Reply(ResultGeometry, payload[:], correlation)
}

// DecodeGeometry reads a ResultGeometry payload.
func DecodeGeometry(payload []byte) (columns, rows int, display bool, ok bool) {
	if len(payload) != GeometrySize || payload[4]&^GeometryDisplay != 0 {
		return 0, 0, false, false
	}
	columns, rows = int(payload[0])|int(payload[1])<<8, int(payload[2])|int(payload[3])<<8
	return columns, rows, payload[4] == GeometryDisplay, columns != 0 && rows != 0
}

// Reply encodes the answer to a request. Only ResultLine and ResultPart carry
// data, at most MaxPart bytes of it.
func Reply(result Result, line []byte, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || !validPayload(result, line) {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: uint16(1 + len(line))}
	message.Payload[0] = byte(result)
	copy(message.Payload[1:], line)
	return message, true
}

// DecodeReply validates a reply; the line aliases the message.
func DecodeReply(message *ipc.Message) (Result, []byte, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize == 0 || message.PayloadSize > 1+MaxPart {
		return 0, nil, false
	}
	result := Result(message.Payload[0])
	line := message.Payload[1:message.PayloadSize]
	if !validPayload(result, line) {
		return 0, nil, false
	}
	return result, line, true
}

// SessionNotice hands over one end of a new session.
func SessionNotice(session handle.ID) (ipc.Message, bool) {
	if session == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindNotification, PayloadSize: 1, HandleCount: 1}
	message.Payload[0] = byte(OperationSession)
	message.Handles[0] = session
	return message, true
}

// DecodeSessionNotice returns the session endpoint a notice carries, as the
// receiver's handle.
func DecodeSessionNotice(message *ipc.Message) (handle.ID, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindNotification ||
		message.Correlation != 0 || message.PayloadSize != 1 || message.Payload[0] != byte(OperationSession) ||
		message.HandleCount != 1 || message.Handles[0] == 0 {
		return 0, false
	}
	return message.Handles[0], true
}

// printable accepts the bytes a prompt may show: printable ASCII only, so a
// client cannot use the terminal service to send control sequences.
func printable(text []byte) bool {
	for _, value := range text {
		if value < ' ' || value > '~' {
			return false
		}
	}
	return true
}
