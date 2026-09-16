package supervisor

import (
	"encoding/binary"
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const (
	LifecycleProtocolID = uint64(0x47494e49544c4946)
	LifecycleVersion    = uint16(4)
	MaxExecutablePath   = 192
	launchGrantSize     = 16
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
	LaunchCapabilityDenied
	LaunchCapabilityInstallFailed
)

type LifecycleEvent struct {
	Operation       LifecycleOperation
	Process         uint64
	Code            uint64
	Executable      [MaxExecutablePath]byte
	ExecutableSize  uint8
	Capabilities    [MaxCapabilities]Capability
	CapabilityCount uint8
	Startup         [128]byte
	StartupSize     uint8
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

func LaunchRequest(service uint64, executable string, capabilities []Capability, correlation uint64) (ipc.Message, bool) {
	return LaunchRequestWithStartup(service, executable, capabilities, nil, correlation)
}

func LaunchRequestWithStartup(service uint64, executable string, capabilities []Capability, startup []byte, correlation uint64) (ipc.Message, bool) {
	if service == 0 || correlation == 0 || !validLaunchPath(executable) || len(capabilities) > MaxCapabilities || len(startup) > 128 || 12+len(executable)+len(startup)+len(capabilities)*launchGrantSize > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	for _, grant := range capabilities {
		if !validCapability(grant) {
			return ipc.Message{}, false
		}
	}
	message := ipc.Message{ProtocolID: LifecycleProtocolID, Version: LifecycleVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(12 + len(executable) + len(startup) + len(capabilities)*launchGrantSize)}
	message.Payload[0] = byte(LifecycleLaunch)
	binary.LittleEndian.PutUint64(message.Payload[1:9], service)
	message.Payload[9] = byte(len(executable))
	message.Payload[10] = byte(len(capabilities))
	message.Payload[11] = byte(len(startup))
	copy(message.Payload[12:], executable)
	copy(message.Payload[12+len(executable):], startup)
	offset := 12 + len(executable) + len(startup)
	for _, grant := range capabilities {
		binary.LittleEndian.PutUint64(message.Payload[offset:], uint64(grant.Handle))
		binary.LittleEndian.PutUint16(message.Payload[offset+8:], uint16(grant.Type))
		binary.LittleEndian.PutUint32(message.Payload[offset+10:], uint32(grant.Rights))
		if grant.Consume {
			message.Payload[offset+14] = 1
		}
		offset += launchGrantSize
	}
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
		if message.Kind != ipc.KindRequest || message.Correlation == 0 || message.PayloadSize < 13 {
			return LifecycleEvent{}, false
		}
		service := binary.LittleEndian.Uint64(message.Payload[1:9])
		size := int(message.Payload[9])
		capabilityCount := int(message.Payload[10])
		startupSize := int(message.Payload[11])
		if service == 0 || size == 0 || size > MaxExecutablePath || startupSize > 128 || capabilityCount > MaxCapabilities || int(message.PayloadSize) != 12+size+startupSize+capabilityCount*launchGrantSize {
			return LifecycleEvent{}, false
		}
		if !validLaunchPathBytes(message.Payload[12 : 12+size]) {
			return LifecycleEvent{}, false
		}
		event := LifecycleEvent{Operation: LifecycleLaunch, Process: service, ExecutableSize: uint8(size), CapabilityCount: uint8(capabilityCount), StartupSize: uint8(startupSize)}
		copy(event.Executable[:], message.Payload[12:12+size])
		copy(event.Startup[:], message.Payload[12+size:12+size+startupSize])
		offset := 12 + size + startupSize
		for index := 0; index < capabilityCount; index++ {
			grant := Capability{
				Handle:  handle.ID(binary.LittleEndian.Uint64(message.Payload[offset:])),
				Type:    handle.Type(binary.LittleEndian.Uint16(message.Payload[offset+8:])),
				Rights:  capability.Rights(binary.LittleEndian.Uint32(message.Payload[offset+10:])),
				Consume: message.Payload[offset+14] == 1,
			}
			if message.Payload[offset+14] > 1 || message.Payload[offset+15] != 0 || !validCapability(grant) {
				return LifecycleEvent{}, false
			}
			event.Capabilities[index] = grant
			offset += launchGrantSize
		}
		return event, true
	case LifecycleLaunchResult:
		if message.Kind != ipc.KindReply || message.Correlation == 0 || message.PayloadSize != 17 {
			return LifecycleEvent{}, false
		}
		process := binary.LittleEndian.Uint64(message.Payload[1:9])
		status := LaunchStatus(binary.LittleEndian.Uint64(message.Payload[9:17]))
		if status > LaunchCapabilityInstallFailed || process == 0 && status == LaunchOK || process != 0 && status != LaunchOK {
			return LifecycleEvent{}, false
		}
		return LifecycleEvent{Operation: LifecycleLaunchResult, Process: process, Code: uint64(status)}, true
	default:
		return LifecycleEvent{}, false
	}
}

func validCapability(grant Capability) bool {
	const allRights = capability.Read | capability.Write | capability.Execute | capability.Transfer | capability.Wait
	return grant.Handle != 0 && grant.Type > handle.TypeInvalid && grant.Type <= handle.TypeIRQ && grant.Rights != 0 && grant.Rights&^allRights == 0
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

func validLaunchPathBytes(path []byte) bool {
	if len(path) < 2 || len(path) > MaxExecutablePath || path[0] != '/' {
		return false
	}
	componentStart := 1
	for index := 1; index < len(path); index++ {
		if path[index] == 0 || path[index] == '/' && path[index-1] == '/' {
			return false
		}
		if path[index] == '/' {
			if equalComponent(path[componentStart:index], '.') || equalComponent(path[componentStart:index], '.', '.') {
				return false
			}
			componentStart = index + 1
		}
	}
	return componentStart < len(path) && !equalComponent(path[componentStart:], '.') && !equalComponent(path[componentStart:], '.', '.')
}

func equalComponent(value []byte, expected ...byte) bool {
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
