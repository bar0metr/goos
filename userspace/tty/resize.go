package tty

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const ResizeRole = "tty-resize"
const SequenceVariable = "GOOS_TTY_SEQUENCE"
const DisplayVariable = "GOOS_TTY_DISPLAY"
const AuthoritativeVariable = "GOOS_TTY_AUTHORITATIVE"

// Snapshot retains the newest geometry, including its borrowed TERM bytes.
// Apply also handles notifications that arrive before the startup snapshot.
type Snapshot struct {
	geometry Geometry
	term     [MaxTerm]byte
	valid    bool
}

func (snapshot *Snapshot) Apply(geometry Geometry) bool {
	if !geometry.Valid() || snapshot.valid && geometry.Sequence <= snapshot.geometry.Sequence {
		return false
	}
	snapshot.geometry = geometry
	snapshot.geometry.Term = snapshot.term[:copy(snapshot.term[:], geometry.Term)]
	snapshot.valid = true
	return true
}

func (snapshot *Snapshot) Current() (Geometry, bool) { return snapshot.geometry, snapshot.valid }
func (snapshot *Snapshot) Reset()                    { *snapshot = Snapshot{} }

// GeometryNotice carries no authority and is independent of input replies.
func GeometryNotice(geometry Geometry) (ipc.Message, bool) {
	message, ok := GeometryStateReply(geometry, 1)
	message.Kind, message.Correlation = ipc.KindNotification, 0
	return message, ok
}

func DecodeGeometryNotice(message *ipc.Message) (Geometry, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindNotification ||
		message.Correlation != 0 || message.HandleCount != 0 || message.PayloadSize < 1 ||
		message.PayloadSize > 1+GeometrySize+MaxTerm || message.Payload[0] != byte(ResultGeometry) {
		return Geometry{}, false
	}
	return DecodeGeometryState(message.Payload[1:message.PayloadSize])
}

// ResizeEndpointNotice restores a suspended foreground parent's receive end.
// PID 1 restricts the attached endpoint to Read|Wait before sending it.
func ResizeEndpointNotice(endpoint handle.ID) (ipc.Message, bool) {
	if endpoint == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindNotification, PayloadSize: 1, HandleCount: 1}
	message.Payload[0], message.Handles[0] = byte(OperationGeometry), endpoint
	return message, true
}

func DecodeResizeEndpointNotice(message *ipc.Message) (handle.ID, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindNotification ||
		message.Correlation != 0 || message.PayloadSize != 1 || message.Payload[0] != byte(OperationGeometry) ||
		message.HandleCount != 1 || message.Handles[0] == 0 {
		return 0, false
	}
	return message.Handles[0], true
}

// RawReleaseNotice removes the session's current foreground resize writer.
func RawReleaseNotice() ipc.Message {
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindNotification, PayloadSize: 1}
	message.Payload[0] = byte(OperationLineMode)
	return message
}

func IsRawReleaseNotice(message *ipc.Message) bool {
	return message.ProtocolID == ProtocolID && message.Version == Version && message.Kind == ipc.KindNotification &&
		message.Correlation == 0 && message.HandleCount == 0 && message.PayloadSize == 1 && message.Payload[0] == byte(OperationLineMode)
}

// GeometryVariable identifies terminal startup entries replaced by PID 1.
func GeometryVariable(entry []byte) bool {
	for _, name := range []string{"COLUMNS", "LINES", "TERM", SequenceVariable, DisplayVariable, AuthoritativeVariable} {
		if len(entry) > len(name) && entry[len(name)] == '=' && string(entry[:len(name)]) == name {
			return true
		}
	}
	return false
}

// ResizeFrameSize fits one atomic native stream write, including zero padding.
const ResizeFrameSize = ipc.WireHeaderSize + 1 + GeometrySize + MaxTerm

func EncodeResizeFrame(geometry Geometry, target []byte) bool {
	if len(target) != ResizeFrameSize {
		return false
	}
	message, valid := GeometryNotice(geometry)
	if !valid {
		return false
	}
	clear(target)
	_, valid = ipc.Encode(message, target)
	return valid
}

func DecodeResizeFrame(frame []byte) (Geometry, bool) {
	if len(frame) != ResizeFrameSize || frame[26] != 0 {
		return Geometry{}, false
	}
	size := ipc.WireHeaderSize + int(binary.LittleEndian.Uint16(frame[24:26]))
	if size > len(frame) {
		return Geometry{}, false
	}
	for _, value := range frame[size:] {
		if value != 0 {
			return Geometry{}, false
		}
	}
	message, valid := ipc.Decode(frame[:size])
	if !valid {
		return Geometry{}, false
	}
	return DecodeGeometryNotice(&message)
}

// GeometryAcknowledgement frees the single outstanding geometry notice slot.
func GeometryAcknowledgement(sequence uint64) ipc.Message {
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindNotification, PayloadSize: 9}
	message.Payload[0] = byte(OperationGeometry)
	binary.LittleEndian.PutUint64(message.Payload[1:9], sequence)
	return message
}

func DecodeGeometryAcknowledgement(message *ipc.Message) (uint64, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindNotification ||
		message.Correlation != 0 || message.HandleCount != 0 || message.PayloadSize != 9 || message.Payload[0] != byte(OperationGeometry) {
		return 0, false
	}
	return binary.LittleEndian.Uint64(message.Payload[1:9]), true
}
