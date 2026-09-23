// Package tty defines the terminal service's session protocol and its line
// discipline. The terminal service is the only process that reads the console;
// a client asks it for lines and receives them edited, or an interrupt, or the
// end of input (ADR 0082).
package tty

import "goos/kernel/ipc"

const (
	ProtocolID = uint64(0x4754545932303031)
	Version    = uint16(1)
	// MaxLine is the longest line a client receives.
	MaxLine = 128
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
	// again with what has been typed, after output has moved the cursor.
	OperationRedraw
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
)

// Request encodes a client request carrying a prompt.
func Request(operation Operation, prompt []byte, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || operation < OperationReadLine || operation > OperationRedraw || len(prompt) > MaxPrompt || !printable(prompt) {
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
	if operation < OperationReadLine || operation > OperationRedraw || !printable(prompt) {
		return 0, nil, false
	}
	return operation, prompt, true
}

// Reply encodes the answer to a request. Only ResultLine carries data.
func Reply(result Result, line []byte, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || result < ResultLine || result > ResultRefused || len(line) > MaxLine || result != ResultLine && len(line) != 0 {
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
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize == 0 || message.PayloadSize > 1+MaxLine {
		return 0, nil, false
	}
	result := Result(message.Payload[0])
	line := message.Payload[1:message.PayloadSize]
	if result < ResultLine || result > ResultRefused || result != ResultLine && len(line) != 0 {
		return 0, nil, false
	}
	return result, line, true
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
