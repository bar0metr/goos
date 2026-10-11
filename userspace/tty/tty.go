// Package tty defines the terminal service's session protocol and its line
// discipline. The terminal service is the only process that reads the console;
// a client asks it for lines and receives them edited, or an interrupt, or the
// end of input (ADR 0082). A client may also switch the session to raw mode,
// in which reads are answered with the bytes typed, unedited and unechoed, for
// a full-screen program in the foreground (ADR 0124).
package tty

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const (
	ProtocolID = uint64(0x4754545932303031)
	// Version 5 adds sequence-tagged geometry notifications.
	Version = uint16(5)
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
	// ResultAccepted acknowledges redraw and mode changes.
	ResultAccepted
	// ResultRefused answers a malformed request, or a read while one is
	// already outstanding.
	ResultRefused
	// ResultPart carries part of a line; the client asks for the rest with
	// OperationContinue, and the last part comes as ResultLine.
	ResultPart
	// ResultRaw carries 1 to MaxPart bytes typed in raw mode.
	ResultRaw
	// ResultGeometry carries uint16 columns and rows, independent display
	// and authoritative flags, then a byte length and printable TERM bytes.
	ResultGeometry
)

// GeometrySize is the fixed header length of a ResultGeometry payload;
// up to MaxTerm printable TERM bytes follow it.
const GeometrySize = 14
const MaxTerm = 64
const MaxRemoteDimension = 1000

// GeometryDisplay identifies a fixed display, not whether dimensions are known.
const GeometryDisplay = byte(1)

// GeometryAuthoritative means children can use the dimensions without probing.
const GeometryAuthoritative = byte(2)

// Geometry describes a terminal without granting access to its transport.
// Term aliases the input when decoded; owners must copy it before reuse.
type Geometry struct {
	Sequence      uint64
	Columns       int
	Rows          int
	Display       bool
	Authoritative bool
	Term          []byte
}

func (geometry Geometry) Valid() bool {
	return geometry.Columns > 0 && geometry.Columns <= 0xffff && geometry.Rows > 0 && geometry.Rows <= 0xffff &&
		len(geometry.Term) <= MaxTerm && printable(geometry.Term)
}

func (geometry Geometry) ValidRemote() bool {
	return geometry.Valid() && geometry.Columns <= MaxRemoteDimension && geometry.Rows <= MaxRemoteDimension
}

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

// validPayload bounds text results and validates typed geometry; other
// results carry no payload.
func validPayload(result Result, line []byte) bool {
	switch {
	case result < ResultLine || result > ResultGeometry:
		return false
	case result == ResultGeometry:
		_, ok := DecodeGeometryState(line)
		return ok
	case !CarriesText(result):
		return len(line) == 0
	case result == ResultPart || result == ResultRaw:
		return len(line) != 0 && len(line) <= MaxPart
	}
	return len(line) <= MaxPart
}

// GeometryReply encodes local geometry. Fixed displays are authoritative;
// serial fallback dimensions are not.
func GeometryReply(columns, rows int, display bool, correlation uint64) (ipc.Message, bool) {
	return GeometryStateReply(Geometry{Columns: columns, Rows: rows, Display: display, Authoritative: display}, correlation)
}

// GeometryStateReply encodes dimensions, independent flags, and bounded TERM.
func GeometryStateReply(geometry Geometry, correlation uint64) (ipc.Message, bool) {
	if !geometry.Valid() {
		return ipc.Message{}, false
	}
	var payload [GeometrySize + MaxTerm]byte
	payload[0], payload[1] = byte(geometry.Columns), byte(geometry.Columns>>8)
	payload[2], payload[3] = byte(geometry.Rows), byte(geometry.Rows>>8)
	if geometry.Display {
		payload[4] |= GeometryDisplay
	}
	if geometry.Authoritative {
		payload[4] |= GeometryAuthoritative
	}
	payload[5] = byte(len(geometry.Term))
	binary.LittleEndian.PutUint64(payload[6:14], geometry.Sequence)
	copy(payload[GeometrySize:], geometry.Term)
	return Reply(ResultGeometry, payload[:GeometrySize+len(geometry.Term)], correlation)
}

// DecodeGeometryState reads a version 5 geometry payload.
func DecodeGeometryState(payload []byte) (Geometry, bool) {
	if len(payload) < GeometrySize || len(payload) > GeometrySize+MaxTerm ||
		payload[4]&^(GeometryDisplay|GeometryAuthoritative) != 0 || int(payload[5]) != len(payload)-GeometrySize {
		return Geometry{}, false
	}
	geometry := Geometry{
		Sequence:      binary.LittleEndian.Uint64(payload[6:14]),
		Columns:       int(payload[0]) | int(payload[1])<<8,
		Rows:          int(payload[2]) | int(payload[3])<<8,
		Display:       payload[4]&GeometryDisplay != 0,
		Authoritative: payload[4]&GeometryAuthoritative != 0,
		Term:          payload[GeometrySize:],
	}
	return geometry, geometry.Valid()
}

// DecodeGeometry returns the dimensions and fixed-display flag.
func DecodeGeometry(payload []byte) (columns, rows int, display bool, ok bool) {
	geometry, ok := DecodeGeometryState(payload)
	return geometry.Columns, geometry.Rows, geometry.Display, ok
}

// Reply encodes a validated result and its bounded payload.
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
