// Package exec defines the bounded shell-to-init native launch protocol.
package exec

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID = uint64(0x4745584543303031)
	Version    = uint16(1)
	MaxPath    = 192
	replySize  = 16
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

type Request struct{ Path, CWD, Arguments []byte }
type Reply struct {
	Process uint64
	Status  uint64
}

func EncodeRequest(request Request, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || !validPath(request.Path) || !validCWD(request.CWD) || len(request.Arguments) > 128 || 4+len(request.Path)+len(request.CWD)+len(request.Arguments) > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(4 + len(request.Path) + len(request.CWD) + len(request.Arguments))}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	message.Payload[0], message.Payload[1] = byte(len(request.Path)), byte(len(request.CWD))
	binary.LittleEndian.PutUint16(message.Payload[2:4], uint16(len(request.Arguments)))
	offset := 4
	copy(message.Payload[offset:], request.Path)
	offset += len(request.Path)
	copy(message.Payload[offset:], request.CWD)
	offset += len(request.CWD)
	copy(message.Payload[offset:], request.Arguments)
	return message, true
}

func DecodeRequest(message ipc.Message) (Request, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < 4 {
		return Request{}, false
	}
	pathSize, cwdSize, argumentSize := int(message.Payload[0]), int(message.Payload[1]), int(binary.LittleEndian.Uint16(message.Payload[2:4]))
	if 4+pathSize+cwdSize+argumentSize != int(message.PayloadSize) || argumentSize > 128 {
		return Request{}, false
	}
	offset := 4
	path := message.Payload[offset : offset+pathSize]
	offset += pathSize
	cwd := message.Payload[offset : offset+cwdSize]
	offset += cwdSize
	if !validPath(path) || !validCWD(cwd) {
		return Request{}, false
	}
	return Request{Path: path, CWD: cwd, Arguments: message.Payload[offset : offset+argumentSize]}, true
}

func validCWD(path []byte) bool { return len(path) == 1 && path[0] == '/' || validPath(path) }

func EncodeReply(reply Reply, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || reply.Process == 0 && reply.Status == 0 || reply.Process != 0 && reply.Status != 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: replySize}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	binary.LittleEndian.PutUint64(message.Payload[0:8], reply.Process)
	binary.LittleEndian.PutUint64(message.Payload[8:16], reply.Status)
	return message, true
}

func DecodeReply(message ipc.Message) (Reply, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindReply || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != replySize {
		return Reply{}, false
	}
	reply := Reply{Process: binary.LittleEndian.Uint64(message.Payload[0:8]), Status: binary.LittleEndian.Uint64(message.Payload[8:16])}
	if reply.Process == 0 && reply.Status == 0 || reply.Process != 0 && reply.Status != 0 {
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
