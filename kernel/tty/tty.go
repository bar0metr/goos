package tty

import "goos/kernel/ipc"

const (
	ProtocolID = uint64(0x47545931)
	Version    = uint16(1)
	MaxLine    = 128
)

type Operation uint8

const (
	OperationWrite Operation = iota + 1
	OperationInput
	OperationSetEcho
	OperationClear
)

func Message(operation Operation, correlation uint64, data []byte) (ipc.Message, bool) {
	if operation < OperationWrite || operation > OperationClear || len(data)+1 > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	kind := ipc.KindNotification
	if correlation != 0 {
		kind = ipc.KindRequest
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: kind, Correlation: correlation, PayloadSize: uint16(len(data) + 1)}
	message.Payload[0] = byte(operation)
	copy(message.Payload[1:], data)
	return message, true
}

func Parse(message ipc.Message) (Operation, []byte, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.PayloadSize == 0 {
		return 0, nil, false
	}
	operation := Operation(message.Payload[0])
	if operation < OperationWrite || operation > OperationClear {
		return 0, nil, false
	}
	return operation, message.Payload[1:message.PayloadSize], true
}

type LineEditor struct {
	data [MaxLine]byte
	size uint16
}

func (editor *LineEditor) Feed(value byte) (line []byte, complete bool, accepted bool) {
	switch value {
	case '\r', '\n':
		return editor.data[:editor.size], true, true
	case 8, 127:
		if editor.size != 0 {
			editor.size--
			editor.data[editor.size] = 0
		}
		return nil, false, true
	default:
		if value < 0x20 || editor.size == MaxLine {
			return nil, false, false
		}
		editor.data[editor.size] = value
		editor.size++
		return nil, false, true
	}
}

// Pending returns the line typed so far.
func (editor *LineEditor) Pending() []byte { return editor.data[:editor.size] }

func (editor *LineEditor) Reset() {
	for index := range editor.data {
		editor.data[index] = 0
	}
	editor.size = 0
}
