// Package exec defines the bounded shell-to-init native launch protocol.
package exec

import (
	"encoding/binary"
	"goos/internal/limits"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/userspace/startup"
)

const (
	ProtocolID = uint64(0x4745584543303031)
	Version    = uint16(11)
	// MaxPath is the native path limit (ADR 0110).
	MaxPath     = limits.PathMax
	MaxStages   = 4
	replySize   = 25
	submitSize  = 5
	startedSize = 9
	headerSize  = 12
	SearchRoot  = "/system/bin/"
)

// Requests are written to an area shared by the shell and PID 1, one slot per
// job in flight; the submit message names the slot. A slot is as large as a
// startup object, which is what a request becomes (ADR 0109). Eight slots are
// seven background jobs and a foreground one (ADR 0111).
const (
	AreaAddress = vm.UserSharedAddress + 16<<20
	Slots       = 8
	SlotSize    = startup.MaxSize
	AreaSize    = uint64(Slots * SlotSize)
)

const flagBackground = 1

func ResolveExecutable(command []byte, target *[MaxPath]byte) ([]byte, bool) {
	if target == nil || len(command) == 0 || len(command) > limits.NameMax || len(SearchRoot)+len(command) > len(target) {
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
	if len(command) > limits.NameMax {
		return nil, false
	}
	for _, value := range command {
		if value == 0 || value == '/' || value == ' ' {
			return nil, false
		}
	}
	if len(command) == 1 && command[0] == '.' || len(command) == 2 && command[0] == '.' && command[1] == '.' {
		return nil, false
	}
	return command, true
}

// List is a validated sequence of strings as a request carries them: each is
// a little-endian u32 length followed by its bytes. It aliases the request.
type List struct {
	data  []byte
	count int
}

// Count is the number of strings in the list.
func (list List) Count() int { return list.count }

// Next returns the string at offset and the offset of the one after it. The
// first string is at offset 0.
func (list List) Next(offset int) ([]byte, int) {
	if offset < 0 || offset+4 > len(list.data) {
		return nil, len(list.data)
	}
	size := int(binary.LittleEndian.Uint32(list.data[offset : offset+4]))
	return list.data[offset+4 : offset+4+size], offset + 4 + size
}

// Stage is one isolated native invocation in a bounded pipeline: the program
// and its arguments, argv[0] first.
type Stage struct {
	Path      []byte
	Arguments List
}

// Request describes one bounded pipeline, as PID 1 decodes it. A non-zero
// ErrorSource is one plus the index of the stage whose stderr feeds the final
// stage. That final stage is a stderr sink outside the stdout chain: it reads
// a private stream and never receives the previous stage's stdout.
//
// A background request runs with an empty standard input. Environment holds
// "NAME=VALUE" strings.
type Request struct {
	Stages      [MaxStages]Stage
	Count       uint8
	ErrorSource uint8
	Background  bool
	CWD         []byte
	Environment List
}

// StageSpec and Spec are what the shell encodes a request from.
type StageSpec struct {
	Path      []byte
	Arguments [][]byte
}

type Spec struct {
	Stages      [MaxStages]StageSpec
	Count       uint8
	ErrorSource uint8
	Background  bool
	CWD         []byte
	// Environment holds "NAME=VALUE" strings.
	Environment [][]byte
}

// Reply reports a finished job. When LaunchStatus is not zero, Stage is the
// index of the stage that could not be launched, so the shell can name it.
type Reply struct {
	Process      uint64
	LaunchStatus uint64
	ExitCode     uint64
	Stage        uint8
}

// EncodeRequest writes spec into one slot and returns the encoded bytes.
func EncodeRequest(spec Spec, target []byte) ([]byte, bool) {
	if spec.Count > MaxStages {
		return nil, false
	}
	builder := NewBuilder(target, spec.CWD, spec.Environment, spec.Background)
	for _, stage := range spec.Stages[:spec.Count] {
		builder.Stage(stage.Path, stage.Arguments)
	}
	return builder.Finish(spec.ErrorSource)
}

// Builder writes a request into a slot a stage at a time, so a caller need not
// keep every stage's arguments at once. The encoding is
//
//	u8 stage count, u8 error source, u8 flags, u8 zero, u32 cwd length,
//	u32 variable count, the cwd, the variables, and per stage a u32 path
//	length, the path, a u32 argument count, and the arguments,
//
// where every variable and argument is a u32 length and its bytes.
type Builder struct {
	target []byte
	offset int
	count  uint8
	failed bool
}

// NewBuilder starts a request in target with its working directory and its
// environment of "NAME=VALUE" strings.
func NewBuilder(target []byte, cwd []byte, environment [][]byte, background bool) Builder {
	if len(target) > SlotSize {
		target = target[:SlotSize]
	}
	builder := Builder{target: target}
	if !validCWD(cwd) || headerSize > len(target) {
		builder.failed = true
		return builder
	}
	target[0], target[1], target[2], target[3] = 0, 0, 0, 0
	if background {
		target[2] = flagBackground
	}
	binary.LittleEndian.PutUint32(target[4:8], uint32(len(cwd)))
	binary.LittleEndian.PutUint32(target[8:12], uint32(len(environment)))
	offset, ok := appendBytes(target, headerSize, cwd)
	for _, variable := range environment {
		if !ok || !validVariable(variable) {
			builder.failed = true
			return builder
		}
		offset, ok = appendString(target, offset, variable)
	}
	builder.offset, builder.failed = offset, !ok
	return builder
}

// Stage adds the next stage: the program's path and its arguments, argv[0]
// first.
func (builder *Builder) Stage(path []byte, arguments [][]byte) {
	if builder.failed || builder.count == MaxStages || !validPath(path) || len(arguments) == 0 || len(arguments[0]) == 0 {
		builder.failed = true
		return
	}
	offset, ok := appendString(builder.target, builder.offset, path)
	if ok && offset+4 <= len(builder.target) {
		binary.LittleEndian.PutUint32(builder.target[offset:offset+4], uint32(len(arguments)))
		offset += 4
	} else {
		ok = false
	}
	for _, argument := range arguments {
		if !ok || !validString(argument) {
			builder.failed = true
			return
		}
		offset, ok = appendString(builder.target, offset, argument)
	}
	builder.offset, builder.failed = offset, !ok
	builder.count++
}

// Finish completes the request; errorSource is as in Request.
func (builder *Builder) Finish(errorSource uint8) ([]byte, bool) {
	if builder.failed || builder.count == 0 || !validErrorSource(errorSource, builder.count) {
		return nil, false
	}
	builder.target[0], builder.target[1] = builder.count, errorSource
	return builder.target[:builder.offset], true
}

func appendString(target []byte, offset int, value []byte) (int, bool) {
	if offset+4 > len(target) {
		return offset, false
	}
	binary.LittleEndian.PutUint32(target[offset:offset+4], uint32(len(value)))
	return appendBytes(target, offset+4, value)
}

func appendBytes(target []byte, offset int, value []byte) (int, bool) {
	if len(value) > len(target)-offset {
		return offset, false
	}
	return offset + copy(target[offset:], value), true
}

// DecodeRequest validates a complete request; the result aliases data.
func DecodeRequest(data []byte) (Request, bool) {
	size := len(data)
	if size < headerSize || size > SlotSize {
		return Request{}, false
	}
	count, errorSource, flags := data[0], data[1], data[2]
	if count == 0 || count > MaxStages || !validErrorSource(errorSource, count) || flags&^flagBackground != 0 || data[3] != 0 {
		return Request{}, false
	}
	cwdSize := uint64(binary.LittleEndian.Uint32(data[4:8]))
	if cwdSize > uint64(size-headerSize) {
		return Request{}, false
	}
	request := Request{Count: count, ErrorSource: errorSource, Background: flags == flagBackground, CWD: data[headerSize : headerSize+int(cwdSize)]}
	if !validCWD(request.CWD) {
		return Request{}, false
	}
	offset := headerSize + int(cwdSize)
	environment, next, ok := decodeList(data, offset, binary.LittleEndian.Uint32(data[8:12]), validVariable)
	if !ok {
		return Request{}, false
	}
	request.Environment, offset = environment, next
	for index := 0; index < int(count); index++ {
		path, next, ok := decodeString(data, offset)
		if !ok || !validPath(path) || next+4 > size {
			return Request{}, false
		}
		arguments, next, ok := decodeList(data, next+4, binary.LittleEndian.Uint32(data[next:next+4]), validString)
		if !ok || arguments.count == 0 {
			return Request{}, false
		}
		if first, _ := arguments.Next(0); len(first) == 0 {
			return Request{}, false
		}
		request.Stages[index] = Stage{Path: path, Arguments: arguments}
		offset = next
	}
	if offset != size {
		return Request{}, false
	}
	return request, true
}

func decodeList(data []byte, offset int, count uint32, valid func([]byte) bool) (List, int, bool) {
	// Every string takes at least its length, so a count the data cannot hold
	// is refused before anything is read.
	if uint64(count)*4 > uint64(len(data)-offset) {
		return List{}, offset, false
	}
	start := offset
	for index := uint32(0); index < count; index++ {
		value, next, ok := decodeString(data, offset)
		if !ok || !valid(value) {
			return List{}, offset, false
		}
		offset = next
	}
	return List{data: data[start:offset], count: int(count)}, offset, true
}

func decodeString(data []byte, offset int) ([]byte, int, bool) {
	if offset+4 > len(data) {
		return nil, offset, false
	}
	size := uint64(binary.LittleEndian.Uint32(data[offset : offset+4]))
	if size > uint64(len(data)-offset-4) {
		return nil, offset, false
	}
	return data[offset+4 : offset+4+int(size)], offset + 4 + int(size), true
}

// validString accepts an argument: bounded, and without a zero byte, as a C
// string must be.
func validString(value []byte) bool {
	if len(value) > startup.MaxString {
		return false
	}
	for _, character := range value {
		if character == 0 {
			return false
		}
	}
	return true
}

// validVariable accepts "NAME=VALUE" with a name that is not empty.
func validVariable(value []byte) bool {
	if !validString(value) {
		return false
	}
	for index, character := range value {
		if character == '=' {
			return index != 0
		}
	}
	return false
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
	binary.LittleEndian.PutUint32(message.Payload[1:5], uint32(size))
	return message, true
}

func DecodeSubmit(message ipc.Message) (uint8, int, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != submitSize {
		return 0, 0, false
	}
	slot, size := message.Payload[0], binary.LittleEndian.Uint32(message.Payload[1:5])
	if slot >= Slots || size < headerSize || uint64(size) > SlotSize {
		return 0, 0, false
	}
	return slot, int(size), true
}

// validErrorSource requires a sink stage plus at least one distinct source.
func validErrorSource(source, count uint8) bool {
	return source == 0 || count >= 2 && source < count
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
