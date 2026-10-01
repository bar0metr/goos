package exec

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

// The launch protocol lets a running application ask PID 1 to run one
// full-screen command in its place and wait for it (ADR 0130). The caller
// writes a request with Builder into a memory object of its own and attaches
// that object to the submit message; PID 1 copies the request out before it
// reads it. The request names a command of /system/bin and nothing else: the
// child's standard streams are the caller's own, which PID 1 kept when it
// launched the caller, and its other authority comes from its manifest.
const (
	LaunchProtocolID = uint64(0x474c4e4348303031)
	LaunchVersion    = uint16(1)
	// MaxLaunchObject bounds the object a caller may attach.
	MaxLaunchObject = SlotSize

	launchSubmitSize = 4
	launchReplySize  = 16
)

// LaunchReply is the result of one launch: why it could not start, or the
// child's exit code.
type LaunchReply struct {
	LaunchStatus uint64
	ExitCode     uint64
}

// EncodeLaunch asks PID 1 to run the request of size bytes at the start of
// object, which moves with the message.
func EncodeLaunch(size int, object handle.ID, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || object == 0 || size < headerSize || size > MaxLaunchObject {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: LaunchProtocolID, Version: LaunchVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: launchSubmitSize, HandleCount: 1}
	message.Handles[0] = object
	binary.LittleEndian.PutUint32(message.Payload[0:4], uint32(size))
	return message, true
}

// DecodeLaunch returns the request size a submit names. The attached object
// is message.Handles[0]; the receiver owns it whether or not the message is
// valid.
func DecodeLaunch(message ipc.Message) (int, bool) {
	if message.ProtocolID != LaunchProtocolID || message.Version != LaunchVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 1 || message.Handles[0] == 0 || message.PayloadSize != launchSubmitSize {
		return 0, false
	}
	size := int(binary.LittleEndian.Uint32(message.Payload[0:4]))
	if size < headerSize || size > MaxLaunchObject {
		return 0, false
	}
	return size, true
}

// EncodeLaunchReply answers a launch. A launch that failed has no exit code.
func EncodeLaunchReply(reply LaunchReply, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || reply.LaunchStatus != 0 && reply.ExitCode != 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: LaunchProtocolID, Version: LaunchVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: launchReplySize}
	binary.LittleEndian.PutUint64(message.Payload[0:8], reply.LaunchStatus)
	binary.LittleEndian.PutUint64(message.Payload[8:16], reply.ExitCode)
	return message, true
}

func DecodeLaunchReply(message ipc.Message) (LaunchReply, bool) {
	if message.ProtocolID != LaunchProtocolID || message.Version != LaunchVersion || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != launchReplySize {
		return LaunchReply{}, false
	}
	reply := LaunchReply{LaunchStatus: binary.LittleEndian.Uint64(message.Payload[0:8]), ExitCode: binary.LittleEndian.Uint64(message.Payload[8:16])}
	if reply.LaunchStatus != 0 && reply.ExitCode != 0 {
		return LaunchReply{}, false
	}
	return reply, true
}

// LaunchCommand returns the /system/bin command a launch request runs. A
// launch runs one program in the foreground: a pipeline, a background job, a
// diverted stderr or a program outside /system/bin is refused.
func (request Request) LaunchCommand() ([]byte, bool) {
	if request.Count != 1 || request.ErrorSource != 0 || request.Background {
		return nil, false
	}
	return CommandFromSearchPath(request.Stages[0].Path)
}
