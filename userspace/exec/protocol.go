// Package exec defines the bounded shell-to-init native launch protocol.
package exec

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/userspace/startup"
)

const (
	ProtocolID  = uint64(0x4745584543303031)
	Version     = uint16(10)
	MaxPath     = 192
	MaxStages   = 4
	replySize   = 25
	submitSize  = 3
	startedSize = 9
	headerSize  = 5
	SearchRoot  = "/system/bin/"
)

// Requests are written to a page shared by the shell and PID 1, one slot per
// job in flight; the submit message names the slot.
const (
	PageAddress = uint64(0x40180000)
	PageSize    = 4096
	Slots       = 4
	SlotSize    = PageSize / Slots
)

const flagBackground = 1

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

// Request describes one bounded pipeline. A non-zero ErrorSource is one plus the
// index of the stage whose stderr feeds the final stage. That final stage is a
// stderr sink outside the stdout chain: it reads a private stream and never
// receives the previous stage's stdout.
//
// A background request runs with an empty standard input.
type Request struct {
	Stages           [MaxStages]Stage
	Count            uint8
	ErrorSource      uint8
	Background       bool
	CWD, Environment []byte
}

// Reply reports a finished job. When LaunchStatus is not zero, Stage is the
// index of the stage that could not be launched, so the shell can name it.
type Reply struct {
	Process      uint64
	LaunchStatus uint64
	ExitCode     uint64
	Stage        uint8
}

// EncodeRequest writes request into one slot and returns the encoded bytes.
func EncodeRequest(request Request, target *[SlotSize]byte) ([]byte, bool) {
	if target == nil || request.Count == 0 || request.Count > MaxStages || !validErrorSource(request.ErrorSource, request.Count) || !validCWD(request.CWD) || len(request.Environment) > 96 {
		return nil, false
	}
	size := headerSize + len(request.CWD) + len(request.Environment)
	for _, stage := range request.Stages[:request.Count] {
		if !validStage(stage, request.Environment) {
			return nil, false
		}
		size += 2 + len(stage.Path) + len(stage.Arguments)
	}
	if size > SlotSize {
		return nil, false
	}
	target[0], target[1], target[2], target[3], target[4] = request.Count, byte(len(request.CWD)), byte(len(request.Environment)), request.ErrorSource, 0
	if request.Background {
		target[4] = flagBackground
	}
	offset := headerSize
	offset += copy(target[offset:], request.CWD)
	offset += copy(target[offset:], request.Environment)
	for _, stage := range request.Stages[:request.Count] {
		target[offset], target[offset+1] = byte(len(stage.Path)), byte(len(stage.Arguments))
		offset += 2
		offset += copy(target[offset:], stage.Path)
		offset += copy(target[offset:], stage.Arguments)
	}
	return target[:size], true
}

// DecodeRequest validates a complete request; the result aliases data.
func DecodeRequest(data []byte) (Request, bool) {
	size := len(data)
	if size < headerSize || size > SlotSize {
		return Request{}, false
	}
	count, cwdSize, environmentSize := data[0], int(data[1]), int(data[2])
	if count == 0 || count > MaxStages || !validErrorSource(data[3], count) || data[4]&^flagBackground != 0 || environmentSize > 96 || headerSize+cwdSize+environmentSize > size {
		return Request{}, false
	}
	request := Request{Count: count, ErrorSource: data[3], Background: data[4] == flagBackground, CWD: data[headerSize : headerSize+cwdSize], Environment: data[headerSize+cwdSize : headerSize+cwdSize+environmentSize]}
	if !validCWD(request.CWD) {
		return Request{}, false
	}
	offset := headerSize + cwdSize + environmentSize
	for index := 0; index < int(count); index++ {
		if offset+2 > size {
			return Request{}, false
		}
		pathSize, argumentSize := int(data[offset]), int(data[offset+1])
		offset += 2
		if offset+pathSize+argumentSize > size {
			return Request{}, false
		}
		stage := Stage{Path: data[offset : offset+pathSize], Arguments: data[offset+pathSize : offset+pathSize+argumentSize]}
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

// EncodeSubmit asks PID 1 to run the request of size bytes in slot.
func EncodeSubmit(slot uint8, size int, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || slot >= Slots || size < headerSize || size > SlotSize {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: submitSize}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	message.Payload[0] = slot
	binary.LittleEndian.PutUint16(message.Payload[1:3], uint16(size))
	return message, true
}

func DecodeSubmit(message ipc.Message) (uint8, int, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != submitSize {
		return 0, 0, false
	}
	slot, size := message.Payload[0], int(binary.LittleEndian.Uint16(message.Payload[1:3]))
	if slot >= Slots || size < headerSize || size > SlotSize {
		return 0, 0, false
	}
	return slot, size, true
}

// validErrorSource requires a sink stage plus at least one distinct source.
func validErrorSource(source, count uint8) bool {
	return source == 0 || count >= 2 && source < count
}

// validStage accepts any canonical absolute path. Where the executable comes
// from, and whether it may run, is PID 1's decision (ADR 0097).
func validStage(stage Stage, environment []byte) bool {
	if !validPath(stage.Path) || len(stage.Arguments) > startup.MaxVector {
		return false
	}
	_, _, ok := startup.DecodeVectors(stage.Arguments, environment)
	return ok
}

func validCWD(path []byte) bool { return len(path) == 1 && path[0] == '/' || validPath(path) }

// EncodeStarted tells the shell a foreground job in slot is running and moves
// to it the writing end of the job's standard input, which the shell feeds
// with the lines the terminal service gives it (ADR 0082). It precedes the
// job's reply and names the job's correlation in its payload: a notification
// carries no correlation of its own, and version 8 put it in the header, where
// the IPC layer refuses it, so the notice was never sent (ADR 0089).
func EncodeStarted(slot uint8, input handle.ID, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || slot >= Slots || input == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindNotification, PayloadSize: startedSize, HandleCount: 1}
	message.Payload[0] = slot
	binary.LittleEndian.PutUint64(message.Payload[1:9], correlation)
	message.Handles[0] = input
	return message, true
}

// DecodeStarted returns the slot, the input handle as received, and the
// correlation of the job it belongs to.
func DecodeStarted(message ipc.Message) (uint8, handle.ID, uint64, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindNotification ||
		message.Correlation != 0 || message.PayloadSize != startedSize || message.HandleCount != 1 || message.Handles[0] == 0 || message.Payload[0] >= Slots {
		return 0, 0, 0, false
	}
	correlation := binary.LittleEndian.Uint64(message.Payload[1:9])
	if correlation == 0 {
		return 0, 0, 0, false
	}
	return message.Payload[0], message.Handles[0], correlation, true
}

func EncodeReply(reply Reply, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || reply.Process == 0 && reply.LaunchStatus == 0 || reply.Process != 0 && reply.LaunchStatus != 0 || !validReplyStage(reply) {
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
	message.Payload[24] = reply.Stage
	return message, true
}

func DecodeReply(message ipc.Message) (Reply, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindReply || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != replySize {
		return Reply{}, false
	}
	reply := Reply{Process: binary.LittleEndian.Uint64(message.Payload[0:8]), LaunchStatus: binary.LittleEndian.Uint64(message.Payload[8:16]), ExitCode: binary.LittleEndian.Uint64(message.Payload[16:24]), Stage: message.Payload[24]}
	if reply.Process == 0 && reply.LaunchStatus == 0 || reply.Process != 0 && reply.LaunchStatus != 0 || reply.Process == 0 && reply.ExitCode != 0 || !validReplyStage(reply) {
		return Reply{}, false
	}
	return reply, true
}

// validReplyStage allows a stage index only where it names a launch failure.
func validReplyStage(reply Reply) bool {
	if reply.LaunchStatus == 0 {
		return reply.Stage == 0
	}
	return reply.Stage < MaxStages
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
