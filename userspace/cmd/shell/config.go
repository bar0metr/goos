//go:build goos_userspace

package main

// The config builtin, over the shell's configuration session (ADR 0074).

import (
	"goos/kernel/ipc"
	"goos/userspace/config"
	"goos/userspace/runtime/sys"
	shellcmd "goos/userspace/shell"
)

// runConfig is a builtin because it administers this session rather than
// processing data: the configuration session belongs to the shell, and an
// external utility would need one of its own.
func runConfig(invocation shellcmd.Invocation) {
	if configHandle == 0 {
		writeError("config: service unavailable\n")
		return
	}
	switch {
	case invocation.Count == 1, invocation.Count == 2 && argumentIs(invocation.Arguments[1], "list"):
		listConfig()
	case invocation.Count == 3 && argumentIs(invocation.Arguments[1], "get"):
		getConfig(invocation.Arguments[2])
	case invocation.Count == 4 && argumentIs(invocation.Arguments[1], "set"):
		setConfig(invocation.Arguments[2], invocation.Arguments[3])
	default:
		commandStatus = shellcmd.StatusUsage
		writeError("usage: config [list] | config get KEY | config set KEY VALUE\n")
	}
}

func listConfig() {
	for cursor := uint32(0); ; {
		reply, ok := callConfig(config.Request{Operation: config.OperationList, Cursor: cursor})
		if !ok {
			writeError("config: service unavailable\n")
			return
		}
		if reply.Status == config.StatusNotFound {
			mark("CONFIG_LIST_OK\n")
			return
		}
		if reply.Status != config.StatusOK || reply.Cursor <= cursor {
			writeConfigStatus(reply.Status)
			return
		}
		writeBytes(reply.Key)
		write(" ")
		writeConfigValue(reply.Value)
		if reply.ReadOnly {
			write(" (read-only)")
		}
		write("\n")
		cursor = reply.Cursor
	}
}

func getConfig(key []byte) {
	reply, ok := callConfig(config.Request{Operation: config.OperationGet, Key: key})
	if !ok {
		writeError("config: service unavailable\n")
		return
	}
	if reply.Status != config.StatusOK {
		writeConfigStatus(reply.Status)
		return
	}
	writeConfigValue(reply.Value)
	write("\n")
	mark("CONFIG_GET_OK\n")
}

// setConfig reads the declared type first and parses the argument against it,
// so a value is converted by what the key is rather than by how the text looks.
func setConfig(key []byte, text []byte) {
	current, ok := callConfig(config.Request{Operation: config.OperationGet, Key: key})
	if !ok {
		writeError("config: service unavailable\n")
		return
	}
	if current.Status != config.StatusOK {
		writeConfigStatus(current.Status)
		return
	}
	value, parsed := parseConfigValue(current.Value.Kind, text)
	if !parsed {
		writeError("config: value does not match the declared type\n")
		return
	}
	reply, ok := callConfig(config.Request{Operation: config.OperationSet, Key: key, Value: value})
	if !ok {
		writeError("config: service unavailable\n")
		return
	}
	if reply.Status != config.StatusOK {
		writeConfigStatus(reply.Status)
		return
	}
	// The shell owns this setting, so a change to it applies now rather than at
	// the next boot.
	loadPrompt()
	mark("CONFIG_SET_OK\n")
}

func parseConfigValue(kind config.Kind, text []byte) (config.Value, bool) {
	switch kind {
	case config.KindBool:
		if argumentIs(text, "true") {
			return config.Bool(true), true
		}
		if argumentIs(text, "false") {
			return config.Bool(false), true
		}
		return config.Value{}, false
	case config.KindUint:
		number, ok := config.ParseUint(text)
		if !ok {
			return config.Value{}, false
		}
		return config.Uint(number), true
	case config.KindText:
		return config.Text(text)
	default:
		return config.Value{}, false
	}
}

func writeConfigValue(value config.Value) {
	switch value.Kind {
	case config.KindBool:
		if value.Uint == 1 {
			write("true")
		} else {
			write("false")
		}
	case config.KindUint:
		writeUint(value.Uint)
	case config.KindText:
		writeBytes(value.Bytes())
	default:
		write("?")
	}
}

func writeConfigStatus(status config.Status) {
	switch status {
	case config.StatusNotFound:
		writeError("config: no such setting\n")
	case config.StatusReadOnly:
		writeError("config: setting is read-only\n")
	case config.StatusTypeMismatch:
		writeError("config: value does not match the declared type\n")
	case config.StatusPermissionDenied:
		writeError("config: not permitted in this session\n")
	default:
		writeError("config: request refused\n")
	}
}

func callConfig(request config.Request) (config.Reply, bool) {
	correlation++
	message, ok := config.EncodeRequest(request, correlation)
	if !ok {
		return config.Reply{}, false
	}
	size, ok := ipc.Encode(message, configWire[:])
	if !ok || sys.IPCSend(configHandle, configWire[:size]) != 0 {
		return config.Reply{}, false
	}
	if sys.IPCWaitReceive(configHandle, configWire[:], 100000) != 0 {
		return config.Reply{}, false
	}
	payloadSize := int(configWire[24]) | int(configWire[25])<<8
	received, valid := ipc.Decode(configWire[:ipc.WireHeaderSize+payloadSize+int(configWire[26])*8])
	if !valid || received.Correlation != correlation {
		return config.Reply{}, false
	}
	reply, valid := config.DecodeReply(received)
	return reply, valid && reply.Operation == request.Operation
}
