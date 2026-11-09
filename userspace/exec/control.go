package exec

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

// The control protocol shares the shell's exec channel to PID 1 for requests
// that launch nothing: listing supervised services, stopping a running job,
// rebooting, and dumping kernel diagnostics. Version 3 adds the diagnostics
// dump, which the shell can no longer make itself because it holds no console
// capability, and the validation build's fault injection (ADR 0089).
const (
	ControlProtocolID = uint64(0x4743544c30303031)
	ControlVersion    = uint16(4)
	// MaxServiceEntries is as many entries as one reply holds: eleven of
	// 22 bytes, after the count, in a 256-byte payload. A system with both
	// consoles and a network runs thirteen services, so a listing asks for
	// the entries from an index on until a reply comes back short.
	MaxServiceEntries = 11
	// MaxServiceIndex bounds the index a listing request may start at.
	MaxServiceIndex  = 32
	MaxServiceName   = 15
	serviceEntrySize = 1 + MaxServiceName + 1 + 1 + 4
)

type ControlOperation uint8

const (
	ControlServices ControlOperation = iota + 1
	ControlReboot
	ControlKill
	// ControlDiagnostics asks PID 1 to have the kernel dump its state to the
	// console. Only a diagnostic build answers it with ControlOK.
	ControlDiagnostics
	// ControlFault asks PID 1 to inject a named validation fault.
	ControlFault
	// ControlPowerOff asks PID 1 to flush storage and switch the machine off
	// (ADR 0092).
	ControlPowerOff
)

// The targets ControlFault may name: an interactive service to kill, the
// persistent filesystem service (ADR 0096), or the kernel stack of PID 1's own
// native call, which the kernel overflows on purpose and which ends the
// machine (ADR 0091).
const (
	FaultTerminal uint8 = iota + 1
	FaultShell
	FaultKernelStack
	FaultFilesystem
	// FaultVFS kills the VFS service, which PID 1 restarts and whose clients
	// it hands new sessions (ADR 0099).
	FaultVFS
	// FaultConfig and FaultLog kill the configuration and log services, which
	// PID 1 restarts with the sessions they need (ADR 0100).
	FaultConfig
	FaultLog
	// FaultBlock kills the block service, which PID 1 restarts with the
	// driver and the filesystem service behind it (ADR 0101).
	FaultBlock
	// FaultDriver kills the storage driver, which PID 1 restarts.
	FaultDriver
	// FaultStorage kills the filesystem service and keeps every restart of it
	// from starting, as a volume that is gone for good would (ADR 0175).
	FaultStorage
	// FaultNetDriver kills the network driver, which PID 1 restarts with a
	// new device binding (ADR 0191).
	FaultNetDriver
	// FaultRemoteSigner closes one SSH worker's signer endpoint.
	FaultRemoteSigner
	// FaultRemoteWorker kills the SSH worker of one running SFTP session.
	FaultRemoteWorker
	// FaultNetService kills netd, which PID 1 restarts; the services that
	// use it get new native sessions (ADR 0210).
	FaultNetService
	// FaultLinuxSockets kills linuxsockd, which PID 1 restarts with new
	// native sessions and new family sessions for running Linux launches.
	FaultLinuxSockets
	// FaultVolume kills the instance serving the first additional volume,
	// which PID 1 replaces while the other volumes keep running (ADR 0222).
	FaultVolume
)

// ControlStatus answers control requests. Remote shutdown returns Accepted
// after storage sync, before PID 1 waits for the requesting session to close.
type ControlStatus uint8

const (
	ControlOK ControlStatus = iota
	ControlSyncFailed
	ControlResetFailed
	ControlInvalid
	ControlNotFound
	// ControlUnsupported answers a power-off the machine cannot perform, for
	// want of the ACPI tables that say how.
	ControlUnsupported
	ControlAccepted
)

type ServiceEntry struct {
	Name     []byte
	State    uint8
	Restarts uint8
	PID      uint32
}

func IsControl(message ipc.Message) bool { return message.ProtocolID == ControlProtocolID }

// EncodeControl carries one operation and its argument. Only ControlKill uses
// the argument, which names the job slot to stop.
func EncodeControl(operation ControlOperation, argument uint8, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || !validControl(operation, argument) {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: 2}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	message.Payload[0], message.Payload[1] = byte(operation), argument
	return message, true
}

func DecodeControl(message ipc.Message) (ControlOperation, uint8, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != 2 {
		return 0, 0, false
	}
	operation, argument := ControlOperation(message.Payload[0]), message.Payload[1]
	if !validControl(operation, argument) {
		return 0, 0, false
	}
	return operation, argument, true
}

// validControl accepts an argument only where the operation takes one: a job
// slot for ControlKill and a service for ControlFault.
func validControl(operation ControlOperation, argument uint8) bool {
	switch operation {
	case ControlServices:
		return argument < MaxServiceIndex
	case ControlReboot, ControlDiagnostics, ControlPowerOff:
		return argument == 0
	case ControlKill:
		return argument < Slots
	case ControlFault:
		return argument >= FaultTerminal && argument <= FaultVolume
	default:
		return false
	}
}

func EncodeServices(entries []ServiceEntry, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || len(entries) > MaxServiceEntries {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: uint16(1 + len(entries)*serviceEntrySize)}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	message.Payload[0] = byte(len(entries))
	for index, entry := range entries {
		if len(entry.Name) == 0 || len(entry.Name) > MaxServiceName {
			return ipc.Message{}, false
		}
		record := message.Payload[1+index*serviceEntrySize : 1+(index+1)*serviceEntrySize]
		record[0] = byte(len(entry.Name))
		copy(record[1:1+MaxServiceName], entry.Name)
		record[1+MaxServiceName], record[2+MaxServiceName] = entry.State, entry.Restarts
		binary.LittleEndian.PutUint32(record[3+MaxServiceName:], entry.PID)
	}
	return message, true
}

func DecodeServices(message ipc.Message, target *[MaxServiceEntries]ServiceEntry) (int, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize == 0 {
		return 0, false
	}
	count := int(message.Payload[0])
	if count > MaxServiceEntries || int(message.PayloadSize) != 1+count*serviceEntrySize {
		return 0, false
	}
	for index := 0; index < count; index++ {
		record := message.Payload[1+index*serviceEntrySize : 1+(index+1)*serviceEntrySize]
		size := int(record[0])
		if size == 0 || size > MaxServiceName {
			return 0, false
		}
		target[index] = ServiceEntry{Name: record[1 : 1+size], State: record[1+MaxServiceName], Restarts: record[2+MaxServiceName],
			PID: binary.LittleEndian.Uint32(record[3+MaxServiceName:])}
	}
	return count, true
}

// EncodeControlStatus answers control requests, including accepted remote
// shutdowns. Local shutdown success still has no reply.
func EncodeControlStatus(status ControlStatus, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || status > ControlAccepted {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: 1}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	message.Payload[0] = byte(status)
	return message, true
}

func DecodeControlStatus(message ipc.Message) (ControlStatus, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != 1 {
		return 0, false
	}
	status := ControlStatus(message.Payload[0])
	return status, status <= ControlAccepted
}

// Volume controls ask PID 1 to mount or unmount an additional volume (ADR
// 0222). They share the control channel and protocol ID, and carry their
// arguments as length-prefixed text: the type, the source and the path of a
// mount; the path of an unmount.
const (
	volumeMount   = byte(0x80)
	volumeUnmount = byte(0x81)
	volumeReply   = byte(0x82)
	// MaxVolumeArgument bounds each text argument.
	MaxVolumeArgument = 120
)

// VolumeCommand is a decoded mount or unmount.
type VolumeCommand struct {
	Mount  bool
	Type   []byte
	Source []byte
	Path   []byte
}

func appendArgument(payload []byte, at int, argument []byte) (int, bool) {
	if len(argument) > MaxVolumeArgument || at+1+len(argument) > len(payload) {
		return at, false
	}
	payload[at] = byte(len(argument))
	copy(payload[at+1:], argument)
	return at + 1 + len(argument), true
}

func takeArgument(payload []byte, at int) ([]byte, int, bool) {
	if at >= len(payload) {
		return nil, at, false
	}
	size := int(payload[at])
	if size > MaxVolumeArgument || at+1+size > len(payload) {
		return nil, at, false
	}
	return payload[at+1 : at+1+size], at + 1 + size, true
}

// EncodeVolumeCommand encodes a mount (with type, source and path) or an
// unmount (with the path alone).
func EncodeVolumeCommand(command VolumeCommand, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || len(command.Path) == 0 || command.Mount == (len(command.Source) == 0) {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest, Correlation: correlation}
	at, ok := 1, true
	if command.Mount {
		message.Payload[0] = volumeMount
		if at, ok = appendArgument(message.Payload[:], at, command.Type); !ok {
			return ipc.Message{}, false
		}
		if at, ok = appendArgument(message.Payload[:], at, command.Source); !ok {
			return ipc.Message{}, false
		}
	} else {
		message.Payload[0] = volumeUnmount
	}
	if at, ok = appendArgument(message.Payload[:], at, command.Path); !ok {
		return ipc.Message{}, false
	}
	message.PayloadSize = uint16(at)
	return message, true
}

// DecodeVolumeCommand decodes a mount or unmount; the slices alias message.
func DecodeVolumeCommand(message *ipc.Message) (VolumeCommand, bool) {
	var command VolumeCommand
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < 2 {
		return command, false
	}
	payload := message.Payload[:message.PayloadSize]
	at, ok := 1, true
	switch payload[0] {
	case volumeMount:
		command.Mount = true
		if command.Type, at, ok = takeArgument(payload, at); !ok {
			return VolumeCommand{}, false
		}
		if command.Source, at, ok = takeArgument(payload, at); !ok || len(command.Source) == 0 {
			return VolumeCommand{}, false
		}
	case volumeUnmount:
	default:
		return VolumeCommand{}, false
	}
	if command.Path, at, ok = takeArgument(payload, at); !ok || len(command.Path) == 0 || at != len(payload) {
		return VolumeCommand{}, false
	}
	return command, true
}

// EncodeVolumeReply answers a volume command with the volume package's
// status and, for a refusal, the VFS status the driver's answer became.
func EncodeVolumeReply(status uint8, detail uint8, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: 3}
	message.Payload[0], message.Payload[1], message.Payload[2] = volumeReply, status, detail
	return message, true
}

func DecodeVolumeReply(message ipc.Message) (uint8, uint8, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != 3 || message.Payload[0] != volumeReply {
		return 0, 0, false
	}
	return message.Payload[1], message.Payload[2], true
}
