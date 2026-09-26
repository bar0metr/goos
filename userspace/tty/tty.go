// Package tty defines the terminal service's session protocol and its line
// discipline. The terminal service is the only process that reads the console;
// a client asks it for lines and receives them edited, or an interrupt, or the
// end of input (ADR 0082).
package tty

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const (
	ProtocolID = uint64(0x4754545932303031)
	Version    = uint16(2)
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
)

// Request encodes a client request carrying a prompt.
func Request(operation Operation, prompt []byte, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || !clientOperation(operation) || len(prompt) > MaxPrompt || !printable(prompt) || operation == OperationContinue && len(prompt) != 0 {
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
	if !clientOperation(operation) || !printable(prompt) || operation == OperationContinue && len(prompt) != 0 {
		return 0, nil, false
	}
	return operation, prompt, true
}

func clientOperation(operation Operation) bool {
	return operation == OperationReadLine || operation == OperationRedraw || operation == OperationContinue
}

// CarriesText reports whether a result may carry part of a line.
func CarriesText(result Result) bool { return result == ResultLine || result == ResultPart }

// Reply encodes the answer to a request. Only ResultLine and ResultPart carry
// data, at most MaxPart bytes of it.
func Reply(result Result, line []byte, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || result < ResultLine || result > ResultPart || len(line) > MaxPart || !CarriesText(result) && len(line) != 0 || result == ResultPart && len(line) == 0 {
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
	if result < ResultLine || result > ResultPart || !CarriesText(result) && len(line) != 0 || result == ResultPart && len(line) == 0 {
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
