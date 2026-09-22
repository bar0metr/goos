//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/kernel/arch/amd64"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/klog"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
)

// consoleThreshold is the lowest level echoed to the serial console; lower
// levels are retained in the ring and read with LogRead.
const consoleThreshold = logfmt.Warn

var (
	systemLog  klog.Ring
	logWire    [kcall.MaxLogRecord]byte
	logText    [kcall.LogText]byte
	kernelName = []byte("kernel")
)

func logRecord(record logfmt.Record) {
	if monotonicClock.Calibrated() {
		record.Time = nowNanos()
	}
	systemLog.Append(record)
	if record.Level >= consoleThreshold {
		size := logfmt.Format(record, logText[:])
		console := amd64.NewCOM1()
		for _, character := range logText[:size] {
			console.WriteByte(character)
		}
	}
}

// kernelLog records an event raised by the kernel itself as process 0.
func kernelLog(level logfmt.Level, message string, fields []byte) {
	logRecord(logfmt.Record{Level: level, Component: kernelName, Message: []byte(message), Fields: fields})
}

func kernelLogUint(level logfmt.Level, message string, key string, value uint64) {
	var buffer [logfmt.MaxFields]byte
	fields := append(append(buffer[:0], key...), '=')
	kernelLog(level, message, logfmt.AppendUint(fields, value))
}

// consoleAuthority resolves the caller's console device capability, which
// authorizes both writing records and reading the log.
func (handler nativeCallHandler) consoleAuthority(device uint64, rights capability.Rights) (*process.Process, bool) {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return nil, false
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(device), handle.TypeDevice, rights)
	return owner, allowed && authority.Object() == 1
}

func (handler nativeCallHandler) LogWrite(device uint64, address uint64, length uint64) kcall.Status {
	owner, allowed := handler.consoleAuthority(device, capability.Write)
	if !allowed {
		return kcall.StatusPermissionDenied
	}
	if length == 0 || length > kcall.MaxLogRecord || !vm.CopyIn(handler.root, address, logWire[:length]) {
		return kcall.StatusInvalidArgument
	}
	record, valid := logfmt.Decode(logWire[:length])
	if !valid {
		return kcall.StatusCorruptData
	}
	record.Process = uint64(owner.ID())
	logRecord(record)
	return kcall.StatusOK
}

// LogRead writes the index-th retained record, oldest first, as one text line
// padded with zeros to kcall.LogText bytes.
func (handler nativeCallHandler) LogRead(device uint64, index uint64, address uint64) kcall.Status {
	if _, allowed := handler.consoleAuthority(device, capability.Read); !allowed {
		return kcall.StatusPermissionDenied
	}
	if index >= klog.Capacity {
		return kcall.StatusNotFound
	}
	record, found := systemLog.At(int(index))
	if !found {
		return kcall.StatusNotFound
	}
	size := logfmt.Format(record, logText[:])
	clear(logText[size:])
	if !vm.CopyOut(handler.root, address, logText[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func kernelLogGrant(message string, name string) {
	var buffer [logfmt.MaxFields]byte
	fields, _ := logfmt.AppendField(buffer[:0], "grant", []byte(name))
	kernelLog(logfmt.Warn, message, fields)
}
