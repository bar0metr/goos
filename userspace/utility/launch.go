//go:build goos_userspace

package utility

import (
	"goos/internal/capability"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	"goos/userspace/runtime/sys"
	"goos/userspace/tty"
)

// launchHandle is the command's launch channel, when its manifest asked for
// GrantLaunch and PID 1 granted it (ADR 0130).
var launchHandle handle.ID

// launchObject is the memory a launch request is written to, created at the
// first launch and kept.
var (
	launchObject handle.ID
	launchMemory []byte
	launchPath   [execproto.MaxPath]byte
)

// launchWait bounds one wait for the reply. A child runs as long as the person
// using it likes, so the wait is repeated until the reply or a dead channel.
const launchWait = 10 * clock.TicksPerSecond

// LaunchAvailable reports whether PID 1 gave this command a launch channel.
func LaunchAvailable() bool { return launchHandle != 0 }

// Launch runs command from /system/bin with argv, argv[0] first, in cwd and
// with environment ("NAME=VALUE" strings), and waits for it. The child reads
// this command's standard input and writes its standard output, so the caller
// must not read input until Launch returns. The result is why the child did
// not start, or its exit code. A command without a launch channel, or whose
// channel failed, gets LaunchCapabilityDenied or LaunchProcessFailed.
func Launch(command []byte, argv [][]byte, cwd []byte, environment [][]byte) (supervisor.LaunchStatus, uint64) {
	if launchHandle == 0 {
		return supervisor.LaunchCapabilityDenied, 0
	}
	if launchMemory == nil {
		object, status := sys.MemoryCreate((execproto.MaxLaunchObject + 4095) / 4096)
		if status != kcall.StatusOK {
			return supervisor.LaunchResourceExhausted, 0
		}
		base, mapped, status := sys.MemoryMapAnywhere(object, true)
		if status != kcall.StatusOK {
			sys.HandleClose(object)
			return supervisor.LaunchResourceExhausted, 0
		}
		launchObject, launchMemory = object, sys.Window(base, mapped)
	}
	path, ok := execproto.ResolveExecutable(command, &launchPath)
	if !ok {
		return supervisor.LaunchNotFound, 0
	}
	builder := execproto.NewBuilder(launchMemory[:execproto.MaxLaunchObject], cwd, environment, false)
	builder.Stage(path, argv)
	request, ok := builder.Finish(0)
	if !ok {
		return supervisor.LaunchArgumentsTooLong, 0
	}
	attached, status := sys.HandleDuplicate(launchObject, capability.Read|capability.Transfer)
	if status != kcall.StatusOK {
		return supervisor.LaunchResourceExhausted, 0
	}
	correlation++
	message, ok := execproto.EncodeLaunch(len(request), attached, correlation)
	size := 0
	if ok {
		size, ok = ipc.Encode(message, wire[:])
	}
	if !ok || sys.IPCSend(launchHandle, wire[:size]) != 0 {
		sys.HandleClose(attached)
		return supervisor.LaunchProcessFailed, 0
	}
	for {
		status := sys.IPCWaitReceive(launchHandle, wire[:], launchWait)
		if status == ipc.StatusTimeout {
			continue
		}
		if status != 0 {
			return supervisor.LaunchProcessFailed, 0
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		received, valid := ipc.Decode(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8])
		if valid {
			if endpoint, ok := tty.DecodeResizeEndpointNotice(&received); ok {
				replaceResize(endpoint)
				continue
			}
		}
		if !valid || received.Correlation != correlation {
			continue
		}
		reply, valid := execproto.DecodeLaunchReply(received)
		if !valid {
			return supervisor.LaunchProcessFailed, 0
		}
		return supervisor.LaunchStatus(reply.LaunchStatus), reply.ExitCode
	}
}
