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
)

// ControlStatus answers a reboot request that could not be carried out; a
// successful reboot is never answered.
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
		return argument >= FaultTerminal && argument <= FaultRemoteSigner
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

// EncodeControlStatus answers a control request. A reboot that succeeds is
// never answered, because the machine resets first.
func EncodeControlStatus(status ControlStatus, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || status > ControlNotFound {
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
	return status, status <= ControlNotFound
}
