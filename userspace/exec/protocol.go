// Package exec defines the bounded shell-to-init native launch protocol.
package exec

import (
	"encoding/binary"
	"goos/kernel/ipc"
	"goos/userspace/startup"
)

const (
	ProtocolID = uint64(0x4745584543303031)
	Version    = uint16(4)
	MaxPath    = 192
	MaxStages  = 4
	replySize  = 24
	SearchRoot = "/system/bin/"
)

func ResolveExecutable(command []byte, target *[MaxPath]byte) ([]byte, bool) {
	if target == nil || len(command) == 0 || len(SearchRoot)+len(command) > len(target) {
		return nil, false
	}
	if len(command) == 1 && command[0] == '.' || len(command) == 2 && command[0] == '.' && command[1] == '.' {
		return nil, false
	}
	for _, value := range command {
		if value == 0 || value == '/' || value == ' ' {
			return nil, false
		}
	}
	copy(target[:], SearchRoot)
	copy(target[len(SearchRoot):], command)
	return target[:len(SearchRoot)+len(command)], true
}

func CommandFromSystemPath(path []byte) ([]byte, bool) {
	if len(path) <= len(SearchRoot) {
		return nil, false
	}
	for index := range SearchRoot {
		if path[index] != SearchRoot[index] {
			return nil, false
		}
	}
	command := path[len(SearchRoot):]
	var target [MaxPath]byte
	resolved, ok := ResolveExecutable(command, &target)
	if !ok || len(resolved) != len(path) {
		return nil, false
	}
	return command, true
}

// Stage is one isolated native invocation in a bounded pipeline.
type Stage struct {
	Path, Arguments []byte
}

type Request struct {
	Stages           [MaxStages]Stage
	Count            uint8
	CWD, Environment []byte
}
type Reply struct {
	Process      uint64
	LaunchStatus uint64
	ExitCode     uint64
}

func EncodeRequest(request Request, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || request.Count == 0 || request.Count > MaxStages || !validCWD(request.CWD) || len(request.Environment) > 96 {
		return ipc.Message{}, false
	}
	size := 4 + len(request.CWD) + len(request.Environment)
	for _, stage := range request.Stages[:request.Count] {
		if !validStage(stage, request.Environment) {
			return ipc.Message{}, false
		}
		size += 2 + len(stage.Path) + len(stage.Arguments)
	}
	if size > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(size)}
	// TinyGo callers require explicit clearing of the attachment fields.
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	message.Payload[0], message.Payload[1], message.Payload[2], message.Payload[3] = request.Count, byte(len(request.CWD)), byte(len(request.Environment)), 0
	offset := 4
	copy(message.Payload[offset:], request.CWD)
	offset += len(request.CWD)
	copy(message.Payload[offset:], request.Environment)
	offset += len(request.Environment)
	for _, stage := range request.Stages[:request.Count] {
		message.Payload[offset], message.Payload[offset+1] = byte(len(stage.Path)), byte(len(stage.Arguments))
		offset += 2
		copy(message.Payload[offset:], stage.Path)
		offset += len(stage.Path)
		copy(message.Payload[offset:], stage.Arguments)
		offset += len(stage.Arguments)
	}
	return message, true
}

func DecodeRequest(message ipc.Message) (Request, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < 4 || message.PayloadSize > ipc.MaxPayload || message.Payload[3] != 0 {
		return Request{}, false
	}
	count, cwdSize, environmentSize := message.Payload[0], int(message.Payload[1]), int(message.Payload[2])
	size := int(message.PayloadSize)
	if count == 0 || count > MaxStages || environmentSize > 96 || 4+cwdSize+environmentSize > size {
		return Request{}, false
	}
	request := Request{Count: count, CWD: message.Payload[4 : 4+cwdSize], Environment: message.Payload[4+cwdSize : 4+cwdSize+environmentSize]}
	if !validCWD(request.CWD) {
		return Request{}, false
	}
	offset := 4 + cwdSize + environmentSize
	for index := 0; index < int(count); index++ {
		if offset+2 > size {
			return Request{}, false
		}
		pathSize, argumentSize := int(message.Payload[offset]), int(message.Payload[offset+1])
		offset += 2
		if offset+pathSize+argumentSize > size {
			return Request{}, false
		}
		stage := Stage{Path: message.Payload[offset : offset+pathSize], Arguments: message.Payload[offset+pathSize : offset+pathSize+argumentSize]}
		if !validStage(stage, request.Environment) {
			return Request{}, false
		}
		request.Stages[index] = stage
		offset += pathSize + argumentSize
	}
	if offset != size {
		return Request{}, false
	}
	return request, true
}

func validStage(stage Stage, environment []byte) bool {
	if _, ok := CommandFromSystemPath(stage.Path); !ok || !validPath(stage.Path) || len(stage.Arguments) > 128 {
		return false
	}
	_, _, ok := startup.DecodeVectors(stage.Arguments, environment)
	return ok
}

func validCWD(path []byte) bool { return len(path) == 1 && path[0] == '/' || validPath(path) }

func EncodeReply(reply Reply, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || reply.Process == 0 && reply.LaunchStatus == 0 || reply.Process != 0 && reply.LaunchStatus != 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: replySize}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	binary.LittleEndian.PutUint64(message.Payload[0:8], reply.Process)
	binary.LittleEndian.PutUint64(message.Payload[8:16], reply.LaunchStatus)
	binary.LittleEndian.PutUint64(message.Payload[16:24], reply.ExitCode)
	return message, true
}

func DecodeReply(message ipc.Message) (Reply, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindReply || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != replySize {
		return Reply{}, false
	}
	reply := Reply{Process: binary.LittleEndian.Uint64(message.Payload[0:8]), LaunchStatus: binary.LittleEndian.Uint64(message.Payload[8:16]), ExitCode: binary.LittleEndian.Uint64(message.Payload[16:24])}
	if reply.Process == 0 && reply.LaunchStatus == 0 || reply.Process != 0 && reply.LaunchStatus != 0 || reply.Process == 0 && reply.ExitCode != 0 {
		return Reply{}, false
	}
	return reply, true
}

func validPath(path []byte) bool {
	if len(path) < 2 || len(path) > MaxPath || path[0] != '/' {
		return false
	}
	start := 1
	for index := 1; index < len(path); index++ {
		if path[index] == 0 || path[index] == '/' && path[index-1] == '/' {
			return false
		}
		if path[index] == '/' {
			if component(path[start:index], ".") || component(path[start:index], "..") {
				return false
			}
			start = index + 1
		}
	}
	return start < len(path) && !component(path[start:], ".") && !component(path[start:], "..")
}

func component(value []byte, expected string) bool {
	if len(value) != len(expected) {
		return false
	}
	for index := range value {
		if value[index] != expected[index] {
			return false
		}
	}
	return true
}
