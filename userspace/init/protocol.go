package supervisor

import (
	"encoding/binary"
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/userspace/startup"
)

const (
	LifecycleProtocolID = uint64(0x47494e49544c4946)
	LifecycleVersion    = uint16(7)
	// MaxProcessName bounds the display name a launch carries. The kernel
	// records it for ps and resolves nothing from it.
	MaxProcessName   = 16
	launchGrantSize  = 16
	launchHeaderSize = 30
)

// Startup blocks travel through a page shared by PID 1 and the kernel instead
// of the launch message. PID 1 writes one slot per launch in flight.
const (
	StagingAddress  = vm.UserStagingAddress
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
	// LaunchStopped is a job stopped while its programs were still being
	// read, before anything was launched (ADR 0108). Only PID 1 reports it.
	LaunchStopped
)

// Reason describes a launch failure for a person reading the console.
func (status LaunchStatus) Reason() string {
	switch status {
	case LaunchOK:
		return "started"
	case LaunchResourceExhausted:
		return "out of resources"
	case LaunchNotFound:
		return "command not found"
	case LaunchAddressSpaceFailed:
		return "cannot create its address space"
	case LaunchInvalidImage:
		return "not a valid executable"
	case LaunchProcessFailed:
		return "process could not be created"
	case LaunchTaskFailed:
		return "task could not be created"
	case LaunchCapabilityDenied:
		return "permission denied"
	case LaunchCapabilityInstallFailed:
		return "capabilities could not be granted"
	case LaunchStopped:
		return "stopped before it started"
	default:
		return "launch failed"
	}
}

// NamesExecutable reports whether the failure is a property of the executable
// being launched, so the message should name it.
func (status LaunchStatus) NamesExecutable() bool {
	return status == LaunchNotFound || status == LaunchInvalidImage || status == LaunchCapabilityDenied
}

// Executable names the bytes of one native image inside an image object. The
// launch request carries this rather than a path: the caller resolves the path
// under its own policy, and the kernel checks only that the handle authorizes
// executing from the image and that the extent lies inside it (ADR 0093).
type Executable struct {
	Image  handle.ID
	Offset uint32
	Size   uint32
}

type LifecycleEvent struct {
	Operation       LifecycleOperation
	Process         uint64
	ProcessHandle   handle.ID
	Code            uint64
	Executable      Executable
	Name            [MaxProcessName]byte
	NameSize        uint8
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

func LaunchRequest(service uint64, executable Executable, name string, capabilities []Capability, correlation uint64) (ipc.Message, bool) {
	return LaunchRequestWithStartup(service, executable, name, capabilities, 0, 0, correlation)
}

// LaunchRequestWithStartup names a startup template of startupSize bytes that
// the caller has written to staging slot startupSlot.
func LaunchRequestWithStartup(service uint64, executable Executable, name string, capabilities []Capability, startupSlot uint8, startupSize uint16, correlation uint64) (ipc.Message, bool) {
	if service == 0 || correlation == 0 || !validExtent(executable) || !validProcessName(name) || len(capabilities) > MaxCapabilities || startupSlot >= StagingSlots || startupSize > StagingSlotSize || launchHeaderSize+len(name)+len(capabilities)*launchGrantSize > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	for _, grant := range capabilities {
		if !validCapability(grant) {
			return ipc.Message{}, false
		}
	}
	message := ipc.Message{ProtocolID: LifecycleProtocolID, Version: LifecycleVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(launchHeaderSize + len(name) + len(capabilities)*launchGrantSize)}
	// Manifest references are payload data, never IPC handle attachments. Set
	// these fields explicitly when the freestanding arena reuses message storage.
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	message.Payload[0] = byte(LifecycleLaunch)
	binary.LittleEndian.PutUint64(message.Payload[1:9], service)
	message.Payload[9] = byte(len(name))
	message.Payload[10] = byte(len(capabilities))
	message.Payload[11] = startupSlot
	binary.LittleEndian.PutUint16(message.Payload[12:14], startupSize)
	binary.LittleEndian.PutUint64(message.Payload[14:22], uint64(executable.Image))
	binary.LittleEndian.PutUint32(message.Payload[22:26], executable.Offset)
	binary.LittleEndian.PutUint32(message.Payload[26:30], executable.Size)
	copy(message.Payload[launchHeaderSize:], name)
	offset := launchHeaderSize + len(name)
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
		executable := Executable{
			Image:  handle.ID(binary.LittleEndian.Uint64(message.Payload[14:22])),
			Offset: binary.LittleEndian.Uint32(message.Payload[22:26]),
			Size:   binary.LittleEndian.Uint32(message.Payload[26:30]),
		}
		if service == 0 || size == 0 || size > MaxProcessName || startupSlot >= StagingSlots || startupSize > StagingSlotSize || capabilityCount > MaxCapabilities || int(message.PayloadSize) != launchHeaderSize+size+capabilityCount*launchGrantSize {
			return LifecycleEvent{}, false
		}
		if !validExtent(executable) || !validProcessNameBytes(message.Payload[launchHeaderSize:launchHeaderSize+size]) {
			return LifecycleEvent{}, false
		}
		event := LifecycleEvent{Operation: LifecycleLaunch, Process: service, Executable: executable, NameSize: uint8(size), CapabilityCount: uint8(capabilityCount), StartupSlot: startupSlot, StartupSize: startupSize}
		copy(event.Name[:], message.Payload[launchHeaderSize:launchHeaderSize+size])
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

// ProcessName returns the event's own bytes. The kernel calls this on every
// launch, and a string conversion would allocate on its leaking heap.
func (event *LifecycleEvent) ProcessName() []byte {
	return event.Name[:event.NameSize]
}

// validExtent checks the form of an extent. Whether the handle authorizes
// execution and whether the extent lies inside the image is for the kernel to
// decide, since only it knows either.
func validExtent(executable Executable) bool {
	return executable.Image != 0 && executable.Size != 0
}

// A process name is a display label: printable, without a separator, so it
// can never be mistaken for a path.
func validProcessName(name string) bool {
	if len(name) == 0 || len(name) > MaxProcessName {
		return false
	}
	for index := 0; index < len(name); index++ {
		if name[index] <= ' ' || name[index] >= 0x7f || name[index] == '/' {
			return false
		}
	}
	return true
}

func validProcessNameBytes(name []byte) bool {
	if len(name) == 0 || len(name) > MaxProcessName {
		return false
	}
	for _, value := range name {
		if value <= ' ' || value >= 0x7f || value == '/' {
			return false
		}
	}
	return true
}
