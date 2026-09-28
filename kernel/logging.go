//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/kernel/arch/amd64"
	kernelconsole "goos/kernel/console"
	"goos/kernel/device"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/klog"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
)

// consoleThreshold is the lowest level echoed to the serial console; lower
// levels are retained in the ring and read with LogRead.
const consoleThreshold = logfmt.Warn

// The kernel leaks every heap allocation, so its own log path uses fixed
// buffers and never converts a string to a slice.
var (
	systemLog     klog.Ring
	logWire       [kcall.MaxLogRecord]byte
	logBoundsWire [kcall.LogBoundsSize]byte
	logText       [kcall.LogText]byte
	logMessage    [logfmt.MaxMessage]byte
	logFields     [logfmt.MaxFields]byte
	logGrant      [logfmt.MaxFields]byte
	kernelName    = []byte("kernel")
)

func logRecord(record logfmt.Record) {
	if value, ok := realtimeClock.Now(&monotonicClock, amd64.ReadTSC()); ok {
		record.Time = value
	}
	systemLog.Append(record)
	if record.Level >= consoleThreshold {
		size := logfmt.Format(record, logText[:])
		console := kernelconsole.New()
		for _, character := range logText[:size] {
			console.WriteByte(character)
		}
	}
}

// kernelLog records an event raised by the kernel itself as process 0.
func kernelLog(level logfmt.Level, message string, fields []byte) {
	size := copy(logMessage[:], message)
	logRecord(logfmt.Record{Level: level, Component: kernelName, Message: logMessage[:size], Fields: fields})
}

func kernelLogUint(level logfmt.Level, message string, key string, value uint64) {
	size := copy(logFields[:], key)
	logFields[size] = '='
	kernelLog(level, message, logfmt.AppendUint(logFields[:size+1], value))
}

// logAuthority reports whether id is the caller's read capability for the log
// ring.
func (handler nativeCallHandler) logAuthority(id uint64) bool {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return false
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Read)
	return allowed && authority.Object() == device.LogObject
}

// consoleAuthority resolves the caller's console device capability, which
// authorizes writing records to the log.
func (handler nativeCallHandler) consoleAuthority(id uint64, rights capability.Rights) (*process.Process, bool) {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return nil, false
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, rights)
	return owner, allowed && device.IsConsole(authority.Object())
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
	if !handler.logAuthority(device) {
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

// LogBounds reports the sequence window the log ring currently retains, so a
// reader that persists records knows which are new and whether any were
// overwritten before it got to them. It takes the same log authority as
// LogRead, because it describes the same records.
func (handler nativeCallHandler) LogBounds(device uint64, address uint64) kcall.Status {
	if !handler.logAuthority(device) {
		return kcall.StatusPermissionDenied
	}
	putUint64(logBoundsWire[0:8], systemLog.Oldest())
	putUint64(logBoundsWire[8:16], systemLog.Total())
	if !vm.CopyOut(handler.root, address, logBoundsWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func kernelLogGrant(message string, name string) {
	size := copy(logGrant[:], name)
	fields, _ := logfmt.AppendField(logFields[:0], "grant", logGrant[:size])
	kernelLog(logfmt.Warn, message, fields)
}
