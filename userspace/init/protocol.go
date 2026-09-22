package supervisor

import (
	"encoding/binary"
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/userspace/startup"
)

const (
	LifecycleProtocolID = uint64(0x47494e49544c4946)
	LifecycleVersion    = uint16(6)
	MaxExecutablePath   = 192
	launchGrantSize     = 16
	launchHeaderSize    = 14
)

// Startup blocks travel through a page shared by PID 1 and the kernel instead
// of the launch message. PID 1 writes one slot per launch in flight.
const (
	StagingAddress  = uint64(0x401ff000)
	StagingSlots    = 4
	StagingSlotSize = startup.Size
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
	ProcessHandle   handle.ID
	Code            uint64
	Executable      [MaxExecutablePath]byte
	ExecutableSize  uint8
	Capabilities    [MaxCapabilities]Capability
	CapabilityCount uint8
	StartupSlot     uint8
	StartupSize     uint16
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
	return LaunchRequestWithStartup(service, executable, capabilities, 0, 0, correlation)
}

// LaunchRequestWithStartup names a startup template of startupSize bytes that
// the caller has written to staging slot startupSlot.
func LaunchRequestWithStartup(service uint64, executable string, capabilities []Capability, startupSlot uint8, startupSize uint16, correlation uint64) (ipc.Message, bool) {
	if service == 0 || correlation == 0 || !validLaunchPath(executable) || len(capabilities) > MaxCapabilities || startupSlot >= StagingSlots || startupSize > StagingSlotSize || launchHeaderSize+len(executable)+len(capabilities)*launchGrantSize > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	for _, grant := range capabilities {
		if !validCapability(grant) {
			return ipc.Message{}, false
		}
	}
	message := ipc.Message{ProtocolID: LifecycleProtocolID, Version: LifecycleVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(launchHeaderSize + len(executable) + len(capabilities)*launchGrantSize)}
	// Manifest references are payload data, never IPC handle attachments. Set
	// these fields explicitly when the freestanding arena reuses message storage.
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	message.Payload[0] = byte(LifecycleLaunch)
	binary.LittleEndian.PutUint64(message.Payload[1:9], service)
	message.Payload[9] = byte(len(executable))
	message.Payload[10] = byte(len(capabilities))
	message.Payload[11] = startupSlot
	binary.LittleEndian.PutUint16(message.Payload[12:14], startupSize)
	copy(message.Payload[launchHeaderSize:], executable)
	offset := launchHeaderSize + len(executable)
	for _, grant := range capabilities {
		binary.LittleEndian.PutUint64(message.Payload[offset:], uint64(grant.Handle))
		binary.LittleEndian.PutUint16(message.Payload[offset+8:], uint16(grant.Type))
		binary.LittleEndian.PutUint32(message.Payload[offset+10:], uint32(grant.Rights))
		message.Payload[offset+14], message.Payload[offset+15] = 0, 0
		if grant.Consume {
			message.Payload[offset+14] = 1
		}
		offset += launchGrantSize
	}
	return message, true
}

func LaunchResult(process uint64, processHandle handle.ID, status LaunchStatus, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || status == LaunchOK && (process == 0 || processHandle == 0) || status != LaunchOK && (process != 0 || processHandle != 0) {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: LifecycleProtocolID, Version: LifecycleVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: 25}
	message.Payload[0] = byte(LifecycleLaunchResult)
	binary.LittleEndian.PutUint64(message.Payload[1:9], process)
	binary.LittleEndian.PutUint64(message.Payload[9:17], uint64(processHandle))
	binary.LittleEndian.PutUint64(message.Payload[17:25], uint64(status))
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
		if message.Kind != ipc.KindRequest || message.Correlation == 0 || message.PayloadSize < launchHeaderSize+2 {
			return LifecycleEvent{}, false
		}
		service := binary.LittleEndian.Uint64(message.Payload[1:9])
		size := int(message.Payload[9])
		capabilityCount := int(message.Payload[10])
		startupSlot := message.Payload[11]
		startupSize := binary.LittleEndian.Uint16(message.Payload[12:14])
		if service == 0 || size == 0 || size > MaxExecutablePath || startupSlot >= StagingSlots || startupSize > StagingSlotSize || capabilityCount > MaxCapabilities || int(message.PayloadSize) != launchHeaderSize+size+capabilityCount*launchGrantSize {
			return LifecycleEvent{}, false
		}
		if !validLaunchPathBytes(message.Payload[launchHeaderSize : launchHeaderSize+size]) {
			return LifecycleEvent{}, false
		}
		event := LifecycleEvent{Operation: LifecycleLaunch, Process: service, ExecutableSize: uint8(size), CapabilityCount: uint8(capabilityCount), StartupSlot: startupSlot, StartupSize: startupSize}
		copy(event.Executable[:], message.Payload[launchHeaderSize:launchHeaderSize+size])
		offset := launchHeaderSize + size
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
		if message.Kind != ipc.KindReply || message.Correlation == 0 || message.PayloadSize != 25 {
			return LifecycleEvent{}, false
		}
		process := binary.LittleEndian.Uint64(message.Payload[1:9])
		processHandle := handle.ID(binary.LittleEndian.Uint64(message.Payload[9:17]))
		status := LaunchStatus(binary.LittleEndian.Uint64(message.Payload[17:25]))
		if status > LaunchCapabilityInstallFailed || status == LaunchOK && (process == 0 || processHandle == 0) || status != LaunchOK && (process != 0 || processHandle != 0) {
			return LifecycleEvent{}, false
		}
		return LifecycleEvent{Operation: LifecycleLaunchResult, Process: process, ProcessHandle: processHandle, Code: uint64(status)}, true
	default:
		return LifecycleEvent{}, false
	}
}

func validCapability(grant Capability) bool {
	const allRights = capability.Read | capability.Write | capability.Execute | capability.Transfer | capability.Wait
	return grant.Handle != 0 && grant.Type > handle.TypeInvalid && grant.Type <= handle.TypeStream && grant.Rights != 0 && grant.Rights&^allRights == 0
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
