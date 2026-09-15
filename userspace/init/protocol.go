package supervisor

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	LifecycleProtocolID = uint64(0x47494e49544c4946)
	LifecycleVersion    = uint16(1)
	MaxExecutablePath   = 192
)

type LifecycleOperation uint8

type LaunchStatus uint64

const (
	LifecycleProcessExited LifecycleOperation = iota + 1
	LifecycleLaunch
	LifecycleLaunchResult
)

const (
	LaunchOK LaunchStatus = iota
	LaunchResourceExhausted
	LaunchNotFound
	LaunchAddressSpaceFailed
	LaunchInvalidImage
	LaunchProcessFailed
	LaunchTaskFailed
)

type LifecycleEvent struct {
	Operation      LifecycleOperation
	Process        uint64
	Code           uint64
	Executable     [MaxExecutablePath]byte
	ExecutableSize uint8
}

func ProcessExited(process uint64, code uint64) (ipc.Message, bool) {
	if process == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: LifecycleProtocolID, Version: LifecycleVersion, Kind: ipc.KindNotification, PayloadSize: 17}
	message.Payload[0] = byte(LifecycleProcessExited)
	binary.LittleEndian.PutUint64(message.Payload[1:9], process)
	binary.LittleEndian.PutUint64(message.Payload[9:17], code)
	return message, true
}

func LaunchRequest(service uint64, executable string, correlation uint64) (ipc.Message, bool) {
	if service == 0 || correlation == 0 || !validLaunchPath(executable) {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: LifecycleProtocolID, Version: LifecycleVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(10 + len(executable))}
	message.Payload[0] = byte(LifecycleLaunch)
	binary.LittleEndian.PutUint64(message.Payload[1:9], service)
	message.Payload[9] = byte(len(executable))
	copy(message.Payload[10:], executable)
	return message, true
}

func LaunchResult(process uint64, status LaunchStatus, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || process == 0 && status == LaunchOK || process != 0 && status != LaunchOK {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: LifecycleProtocolID, Version: LifecycleVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: 17}
	message.Payload[0] = byte(LifecycleLaunchResult)
	binary.LittleEndian.PutUint64(message.Payload[1:9], process)
	binary.LittleEndian.PutUint64(message.Payload[9:17], uint64(status))
	return message, true
}

func DecodeLifecycle(message ipc.Message) (LifecycleEvent, bool) {
	if message.ProtocolID != LifecycleProtocolID || message.Version != LifecycleVersion || message.HandleCount != 0 {
		return LifecycleEvent{}, false
	}
	switch LifecycleOperation(message.Payload[0]) {
	case LifecycleProcessExited:
		if message.Kind != ipc.KindNotification || message.Correlation != 0 || message.PayloadSize != 17 {
			return LifecycleEvent{}, false
		}
		process := binary.LittleEndian.Uint64(message.Payload[1:9])
		if process == 0 {
			return LifecycleEvent{}, false
		}
		return LifecycleEvent{Operation: LifecycleProcessExited, Process: process, Code: binary.LittleEndian.Uint64(message.Payload[9:17])}, true
	case LifecycleLaunch:
		if message.Kind != ipc.KindRequest || message.Correlation == 0 || message.PayloadSize < 11 {
			return LifecycleEvent{}, false
		}
		service := binary.LittleEndian.Uint64(message.Payload[1:9])
		size := int(message.Payload[9])
		if service == 0 || size == 0 || size > MaxExecutablePath || int(message.PayloadSize) != 10+size {
			return LifecycleEvent{}, false
		}
		executable := string(message.Payload[10 : 10+size])
		if !validLaunchPath(executable) {
			return LifecycleEvent{}, false
		}
		event := LifecycleEvent{Operation: LifecycleLaunch, Process: service, ExecutableSize: uint8(size)}
		copy(event.Executable[:], executable)
		return event, true
	case LifecycleLaunchResult:
		if message.Kind != ipc.KindReply || message.Correlation == 0 || message.PayloadSize != 17 {
			return LifecycleEvent{}, false
		}
		process := binary.LittleEndian.Uint64(message.Payload[1:9])
		status := LaunchStatus(binary.LittleEndian.Uint64(message.Payload[9:17]))
		if status > LaunchTaskFailed || process == 0 && status == LaunchOK || process != 0 && status != LaunchOK {
			return LifecycleEvent{}, false
		}
		return LifecycleEvent{Operation: LifecycleLaunchResult, Process: process, Code: uint64(status)}, true
	default:
		return LifecycleEvent{}, false
	}
}

func (event LifecycleEvent) ExecutablePath() string {
	return string(event.Executable[:event.ExecutableSize])
}

func validLaunchPath(path string) bool {
	if len(path) < 2 || len(path) > MaxExecutablePath || path[0] != '/' {
		return false
	}
	componentStart := 1
	for index := 1; index < len(path); index++ {
		if path[index] == 0 || path[index] == '/' && (index == 0 || path[index-1] == '/') {
			return false
		}
		if path[index] == '/' {
			if path[componentStart:index] == "." || path[componentStart:index] == ".." {
				return false
			}
			componentStart = index + 1
		}
	}
	return componentStart < len(path) && path[componentStart:] != "." && path[componentStart:] != ".."
}
