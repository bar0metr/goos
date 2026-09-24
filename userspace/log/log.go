// Package log emits structured events to the system log. The kernel stamps
// each record with the time and the caller's process ID; warnings and errors
// are also echoed to the console.
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

var (
	console   handle.ID
	component string
	wire      [logfmt.MaxWire]byte
	fields    [logfmt.MaxFields]byte
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
	encoded := fields[:0]
	for _, field := range values {
		var digits [20]byte
		value := field.Value
		if field.Kind == kindUint {
			value = logfmt.AppendUint(digits[:0], field.Uint)
		}
		var ok bool
		if encoded, ok = logfmt.AppendField(encoded, field.Key, value); !ok {
			return false
		}
	}
	size, ok := logfmt.Encode(level, sys.Bytes(component), sys.Bytes(message), encoded, wire[:])
	return ok && sys.LogWrite(console, wire[:size]) == kcall.StatusOK
}
