// Package log emits structured events to the system log. The kernel stamps
// each record with the time and the caller's process ID; warnings and errors
// are also echoed to the console.
//
// Every event is assembled in buffers on the caller's stack and handed to the
// kernel in one write, so events from different goroutines or threads of a
// process are each whole, in whatever order they arrive. Init runs once,
// before the process starts a second goroutine or thread.
package log

import (
	"goos/internal/logfmt"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

type Field struct {
	Key   string
	Value []byte
	Uint  uint64
	Kind  uint8
}

const (
	kindText uint8 = iota
	kindUint
)

// console and component are set by Init and only read afterwards.
var (
	console   handle.ID
	component string
)

// Init names the calling component and the console device that authorizes
// logging.
func Init(device handle.ID, name string) {
	console, component = device, name
}

func Str(key string, value string) Field {
	return Field{Key: key, Value: sys.Bytes(value)}
}

func Bytes(key string, value []byte) Field { return Field{Key: key, Value: value} }

func Uint(key string, value uint64) Field { return Field{Key: key, Uint: value, Kind: kindUint} }

func Info(message string, values ...Field) bool  { return emit(logfmt.Info, message, values) }
func Warn(message string, values ...Field) bool  { return emit(logfmt.Warn, message, values) }
func Error(message string, values ...Field) bool { return emit(logfmt.Error, message, values) }

func emit(level logfmt.Level, message string, values []Field) bool {
	var wire [logfmt.MaxWire]byte
	size, ok := encode(level, component, message, values, &wire)
	return ok && sys.LogWrite(console, wire[:size]) == kcall.StatusOK
}

// encode builds one event's wire record in wire and returns its size.
func encode(level logfmt.Level, name, message string, values []Field, wire *[logfmt.MaxWire]byte) (int, bool) {
	var fields [logfmt.MaxFields]byte
	encoded := fields[:0]
	for _, field := range values {
		var digits [20]byte
		value := field.Value
		if field.Kind == kindUint {
			value = logfmt.AppendUint(digits[:0], field.Uint)
		}
		var ok bool
		if encoded, ok = logfmt.AppendField(encoded, field.Key, value); !ok {
			return 0, false
		}
	}
	return logfmt.Encode(level, sys.Bytes(name), sys.Bytes(message), encoded, wire[:])
}
