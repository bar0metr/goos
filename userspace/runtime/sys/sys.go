// Package sys is the one place userspace crosses into the kernel. Every
// native call the runtime's assembly stubs provide (userspace/runtime/amd64.S)
// is declared here once and wrapped in a typed function that takes handles,
// slices and strings and returns a kcall.Status, so programs and services never
// build raw descriptors or convert pointers themselves (ADR 0095).
//
// The wrappers do not allocate. Descriptors the kernel reads live in package
// scratch variables: every userspace process runs a single task, so no two
// calls are in flight at once.
//
// Every unsafe conversion in this package passes the kernel the address of Go
// memory that stays reachable for the whole call; the kernel copies through
// CopyIn and CopyOut and never keeps the address past the call.
package sys

import (
	"encoding/binary"
	"goos/internal/timekeeping"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"unsafe"
)

// Scratch descriptors. See the package comment for why package state is safe.
var (
	pairWire        [2]uint64
	transferWire    [3]uint64
	receiveWire     [3]uint64
	valueWire       uint64
	boundsWire      [2]uint64
	imageWire       [2]uint64
	dmaWire         [3]uint64
	waitWire        [2]uint64
	timeWire        [kcall.ClockTimeSize]byte
	consoleByteBox  [1]byte
	consoleInfoWire [kcall.ConsoleInfoSize]byte
)

type ConsoleInfo struct {
	Columns uint32
	Rows    uint32
	Flags   uint32
}

func status(value uint64) kcall.Status { return kcall.Status(value) }

// address is the user virtual address of the first byte of data, or zero for
// an empty slice, which every call treats as an invalid pointer. The pages of
// data are touched first.
func address(data []byte) unsafe.Pointer {
	if len(data) == 0 {
		return nil
	}
	touch(data)
	return unsafe.Pointer(&data[0])
}

// touch reads one byte of every page data spans. A program read from a file
// gets its pages when it first touches them (ADR 0121), and the kernel copies
// a call's buffers without taking those faults, so a buffer it reads or
// fills, a string constant included, must already be in place (ADR 0175).
func touch(data []byte) {
	if len(data) == 0 {
		return
	}
	sum := data[0] ^ data[len(data)-1]
	start := uintptr(unsafe.Pointer(&data[0]))
	for offset := (pageSize - start%pageSize) % pageSize; offset < uintptr(len(data)); offset += pageSize {
		sum ^= data[offset]
	}
	touched ^= sum
}

// touched keeps the reads touch makes from being optimized away.
var touched byte

const pageSize = 4096

// Exit ends the calling process with code. It does not return.
func Exit(code uint64) { processExit(code) }

// ConsoleWrite writes data to the console through a console capability.
func ConsoleWrite(device handle.ID, data []byte) kcall.Status {
	return status(consoleWrite(uint64(device), address(data), uintptr(len(data))))
}

// ConsoleWriteString is ConsoleWrite for text, without copying it.
func ConsoleWriteString(device handle.ID, text string) kcall.Status {
	return ConsoleWrite(device, Bytes(text))
}

// ConsoleRead takes one typed byte from the console, or NotFound when none is
// pending.
func ConsoleRead(device handle.ID) (byte, kcall.Status) {
	consoleByteBox[0] = 0
	result := status(consoleRead(uint64(device), unsafe.Pointer(&consoleByteBox[0]), 1))
	return consoleByteBox[0], result
}

// ConsoleInterrupt reports and consumes a pending Ctrl-C.
func ConsoleInterrupt(device handle.ID) kcall.Status {
	return status(consoleInterrupt(uint64(device)))
}

// ConsoleInformation returns terminal geometry and availability flags.
func ConsoleInformation(device handle.ID) (ConsoleInfo, kcall.Status) {
	clear(consoleInfoWire[:])
	consoleInfoWire = [kcall.ConsoleInfoSize]byte{}
	result := status(consoleInfo(uint64(device), unsafe.Pointer(&consoleInfoWire[0]), uintptr(len(consoleInfoWire))))
	return ConsoleInfo{
		Columns: uint32(consoleInfoWire[0]) | uint32(consoleInfoWire[1])<<8 | uint32(consoleInfoWire[2])<<16 | uint32(consoleInfoWire[3])<<24,
		Rows:    uint32(consoleInfoWire[4]) | uint32(consoleInfoWire[5])<<8 | uint32(consoleInfoWire[6])<<16 | uint32(consoleInfoWire[7])<<24,
		Flags:   uint32(consoleInfoWire[8]) | uint32(consoleInfoWire[9])<<8 | uint32(consoleInfoWire[10])<<16 | uint32(consoleInfoWire[11])<<24,
	}, result
}

// LogWrite appends one encoded record to the kernel log.
func LogWrite(device handle.ID, record []byte) kcall.Status {
	return status(logWrite(uint64(device), address(record), uintptr(len(record))))
}

// LogRead copies the text of the record index places past the oldest one.
func LogRead(device handle.ID, index uint64, target *[kcall.LogText]byte) kcall.Status {
	touch(target[:])
	return status(logRead(uint64(device), index, unsafe.Pointer(&target[0])))
}

// LogBounds reports the sequence numbers of the oldest retained record and one
// past the newest.
func LogBounds(device handle.ID) (uint64, uint64, kcall.Status) {
	boundsWire[0], boundsWire[1] = 0, 0
	result := status(logBounds(uint64(device), unsafe.Pointer(&boundsWire[0])))
	return boundsWire[0], boundsWire[1], result
}

// Diagnostics asks a diagnostic build to dump the caller's state.
func Diagnostics(device handle.ID) kcall.Status {
	return status(diagnostics(uint64(device), 0))
}

// DiagnosticsTrace is Diagnostics that also traces how words is mapped, to
// find out whether the memory a process believes it holds is what it holds.
func DiagnosticsTrace(device handle.ID, words []uint64) kcall.Status {
	if len(words) == 0 {
		return Diagnostics(device)
	}
	touch(unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8))
	return status(diagnostics(uint64(device), uintptr(unsafe.Pointer(&words[0]))))
}

// IPCSend sends one encoded message.
func IPCSend(channel handle.ID, wire []byte) kcall.Status {
	return status(ipcSend(uint64(channel), address(wire), uintptr(len(wire))))
}

// IPCReceive takes one message into buffer if one is queued, without waiting.
func IPCReceive(channel handle.ID, buffer []byte) kcall.Status {
	return status(ipcReceive(uint64(channel), address(buffer), uintptr(len(buffer))))
}

// IPCWaitReceive takes one message into buffer, waiting up to timeout ticks
// for it.
func IPCWaitReceive(channel handle.ID, buffer []byte, timeout uint64) kcall.Status {
	receiveWire[0], receiveWire[1], receiveWire[2] = uint64(uintptr(address(buffer))), uint64(len(buffer)), timeout
	return status(ipcWaitReceive(uint64(channel), unsafe.Pointer(&receiveWire[0]), uintptr(len(receiveWire)*8)))
}

// ChannelCreate creates a channel and returns its two endpoints.
func ChannelCreate() (handle.ID, handle.ID, kcall.Status) {
	pairWire[0], pairWire[1] = 0, 0
	result := status(channelCreate(unsafe.Pointer(&pairWire[0]), uintptr(len(pairWire)*8)))
	return handle.ID(pairWire[0]), handle.ID(pairWire[1]), result
}

// StreamCreate creates a byte stream and returns its reading and writing ends.
func StreamCreate() (handle.ID, handle.ID, kcall.Status) {
	pairWire[0], pairWire[1] = 0, 0
	result := status(streamCreate(unsafe.Pointer(&pairWire[0]), uintptr(len(pairWire)*8)))
	return handle.ID(pairWire[0]), handle.ID(pairWire[1]), result
}

// StreamRead reads what the stream holds, up to len(target) bytes, and
// reports how many arrived.
func StreamRead(stream handle.ID, target []byte) (int, kcall.Status) {
	transferWire[0], transferWire[1], transferWire[2] = uint64(uintptr(address(target))), uint64(len(target)), 0
	result := status(streamRead(uint64(stream), unsafe.Pointer(&transferWire[0]), uintptr(len(transferWire)*8)))
	return int(transferWire[2]), result
}

// StreamWrite writes as much of data as the stream has room for and reports
// how much that was.
func StreamWrite(stream handle.ID, data []byte) (int, kcall.Status) {
	transferWire[0], transferWire[1], transferWire[2] = uint64(uintptr(address(data))), uint64(len(data)), 0
	result := status(streamWrite(uint64(stream), unsafe.Pointer(&transferWire[0]), uintptr(len(transferWire)*8)))
	return int(transferWire[2]), result
}

// StreamWait waits up to timeout ticks for the stream to become ready.
func StreamWait(stream handle.ID, timeout uint64) kcall.Status {
	return status(streamWait(uint64(stream), timeout))
}

// HandleClose closes a handle.
func HandleClose(id handle.ID) kcall.Status { return status(handleClose(uint64(id))) }

// HandleInfo describes slot of the caller's own handle table.
func HandleInfo(slot uint64, target *[kcall.HandleInfoSize]byte) kcall.Status {
	touch(target[:])
	return status(handleInfo(slot, unsafe.Pointer(&target[0])))
}

// ProcessWait waits up to timeout ticks for a process to exit and returns its
// exit code.
func ProcessWait(process handle.ID, timeout uint64) (uint64, kcall.Status) {
	valueWire = 0
	result := status(processWait(uint64(process), unsafe.Pointer(&valueWire), timeout))
	return valueWire, result
}

// ProcessKill stops a process through a process handle with Write.
func ProcessKill(process handle.ID) kcall.Status { return status(processKill(uint64(process))) }

// ProcessInfo fills target with the record of the index-th process.
func ProcessInfo(index uint64, target *[kcall.ProcessInfoSize]byte) kcall.Status {
	touch(target[:])
	return status(processInfo(index, unsafe.Pointer(&target[0])))
}

// StartupRead copies the first len(target) bytes of the caller's startup
// object, zeros past its end.
func StartupRead(target []byte) kcall.Status {
	if len(target) == 0 {
		return kcall.StatusInvalidArgument
	}
	touch(target)
	return status(startupRead(unsafe.Pointer(&target[0]), uintptr(len(target))))
}

// SystemInfo fills target with the kernel's system summary.
func SystemInfo(target *[kcall.SystemInfoSize]byte) kcall.Status {
	touch(target[:])
	return status(systemInfoRead(unsafe.Pointer(&target[0]), uintptr(len(target))))
}

// CPUInfo fills target with the immutable CPU summary record.
func CPUInfo(target *[kcall.CPUInfoSize]byte) kcall.Status {
	touch(target[:])
	return status(cpuInfoRead(unsafe.Pointer(&target[0]), uintptr(len(target))))
}

// CPUEntry fills target with the index-th logical CPU record.
func CPUEntry(index uint64, target *[kcall.CPUEntrySize]byte) kcall.Status {
	touch(target[:])
	return status(cpuEntryRead(index, unsafe.Pointer(&target[0])))
}

// CPUCacheInfo fills target with the index-th deterministic cache record.
func CPUCacheInfo(index uint64, target *[kcall.CPUCacheInfoSize]byte) kcall.Status {
	touch(target[:])
	return status(cpuCacheInfoRead(index, unsafe.Pointer(&target[0])))
}

// CPURuntime fills target with the index-th processor's scheduler counters.
func CPURuntime(index uint64, target *[kcall.CPURuntimeSize]byte) kcall.Status {
	touch(target[:])
	return status(cpuRuntimeRead(index, unsafe.Pointer(&target[0])))
}

// TaskAffinity returns the processors a process's tasks may run on, after
// limiting them to mask when mask is not zero. Handle zero names the calling
// task.
func TaskAffinity(process handle.ID, mask uint64) (uint64, kcall.Status) {
	var wire [kcall.AffinityMaskSize]byte
	touch(wire[:])
	result := status(taskAffinity(uint64(process), mask, unsafe.Pointer(&wire[0])))
	return binary.LittleEndian.Uint64(wire[:]), result
}

// ClockRead reads the monotonic timer in ticks.
func ClockRead() (uint64, kcall.Status) {
	valueWire = 0
	result := status(clockRead(unsafe.Pointer(&valueWire), 8))
	return valueWire, result
}

// ClockReadNanos reads the monotonic clock in nanoseconds.
func ClockReadNanos() (uint64, kcall.Status) {
	valueWire = 0
	result := status(clockReadNanos(unsafe.Pointer(&valueWire), 8))
	return valueWire, result
}

// ClockNow reads a fixed-width native MONOTONIC or REALTIME timestamp.
func ClockNow(clock timekeeping.Clock) (timekeeping.Time, kcall.Status) {
	clear(timeWire[:])
	timeWire = [kcall.ClockTimeSize]byte{}
	result := status(clockNow(uint64(clock), unsafe.Pointer(&timeWire[0]), uintptr(len(timeWire))))
	if result != kcall.StatusOK {
		return timekeeping.Time{}, result
	}
	return timekeeping.Time{Seconds: int64(le64(timeWire[0:8])), Nanoseconds: uint32(timeWire[8]) | uint32(timeWire[9])<<8 | uint32(timeWire[10])<<16 | uint32(timeWire[11])<<24}, result
}

// ClockSet is the reserved realtime setter. It requires a handle carrying the
// dedicated CLOCK_SET right; the current kernel returns Unsupported even to
// an authorized caller.
func ClockSet(authority handle.ID, value timekeeping.Time) kcall.Status {
	clear(timeWire[:])
	seconds := uint64(value.Seconds)
	for index := 0; index < 8; index++ {
		timeWire[index] = byte(seconds >> uint(index*8))
	}
	for index := 0; index < 4; index++ {
		timeWire[8+index] = byte(value.Nanoseconds >> uint(index*8))
	}
	return status(clockSet(uint64(authority), unsafe.Pointer(&timeWire[0]), uintptr(len(timeWire))))
}

// PortRead reads an I/O port the device capability covers. specification is
// the port offset in its low 16 bits and the access width in the next eight.
func PortRead(device handle.ID, specification uint64) (uint64, kcall.Status) {
	valueWire = 0
	result := status(portRead(uint64(device), specification, unsafe.Pointer(&valueWire)))
	return valueWire, result
}

// PortWrite writes an I/O port the device capability covers.
func PortWrite(device handle.ID, specification uint64, value uint64) kcall.Status {
	return status(portWrite(uint64(device), specification, value))
}

// IRQWait blocks until the interrupt line the capability names fires.
func IRQWait(irq handle.ID) kcall.Status { return status(irqWait(uint64(irq))) }

// SystemReset resets the machine through the system-control capability.
func SystemReset(device handle.ID) kcall.Status { return status(systemReset(uint64(device))) }

// SystemPowerOff switches the machine off through the system-control
// capability.
func SystemPowerOff(device handle.ID) kcall.Status { return status(systemPowerOff(uint64(device))) }

// ValidationFault overflows the caller's kernel stack on purpose; only the
// validation build accepts it.
func ValidationFault(device handle.ID) kcall.Status { return status(validationFault(uint64(device))) }

// Interest is one object a WaitAny call watches. Its layout is the kernel's
// wire record: the handle, then the event mask.
type Interest struct {
	Handle handle.ID
	Events kcall.WaitEvent
}

// WaitAny blocks until one of the first count interests is ready or timeout
// ticks elapse, and returns the mask of ready interests.
func WaitAny(interests *[kcall.WaitAnyMaxObjects]Interest, count int, timeout uint64) (uint64, kcall.Status) {
	if interests == nil || count < 0 || count > kcall.WaitAnyMaxObjects {
		return 0, kcall.StatusInvalidArgument
	}
	waitWire[0], waitWire[1] = timeout, 0
	touch(unsafe.Slice((*byte)(unsafe.Pointer(&interests[0])), count*16))
	// SAFETY: Interest is two 8-byte fields with no padding, which is the
	// kernel's 16-byte interest record, and the array outlives the call.
	result := status(waitAny(unsafe.Pointer(&interests[0]), uintptr(count), unsafe.Pointer(&waitWire[0])))
	return waitWire[1], result
}
