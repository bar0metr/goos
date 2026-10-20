//go:build goos_userspace

package utility

import (
	"goos/internal/capability"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	"goos/userspace/runtime/sys"
	"goos/userspace/tty"
)

// launcher is the command's launch channel, when its manifest asked for
// GrantLaunch and PID 1 granted it (ADR 0130), with the memory a launch
// request is written to, created at the first launch and kept, and the path
// buffer. The session's holder owns the memory and the path.
var launcher struct {
	session[nativeIPC]
	object handle.ID
	memory []byte
	path   [execproto.MaxPath]byte
}

// launchWait bounds one wait for the reply. A child runs as long as the person
// using it likes, so the wait is repeated until the reply or a dead channel.
const launchWait = 10 * clock.TicksPerSecond

// LaunchAvailable reports whether PID 1 gave this command a launch channel.
func LaunchAvailable() bool { return launcher.bound() != 0 }

// Launch runs command from /system/bin with argv, argv[0] first, in cwd and
// with environment ("NAME=VALUE" strings), and waits for it. The child reads
// this command's standard input and writes its standard output, so the caller
// must not read input until Launch returns. The result is why the child did
// not start, or its exit code. A command without a launch channel, or whose
// channel failed, gets LaunchCapabilityDenied or LaunchProcessFailed.
//
// The launch channel is held until the child exits: a second goroutine's
// Launch waits for the first child.
func Launch(command []byte, argv [][]byte, cwd []byte, environment [][]byte) (supervisor.LaunchStatus, uint64) {
	if launcher.bound() == 0 {
		return supervisor.LaunchCapabilityDenied, 0
	}
	correlation := launcher.begin()
	defer launcher.end()
	if launcher.memory == nil {
		object, status := sys.MemoryCreate((execproto.MaxLaunchObject + 4095) / 4096)
		if status != kcall.StatusOK {
			return supervisor.LaunchResourceExhausted, 0
		}
		base, mapped, status := sys.MemoryMapAnywhere(object, true)
		if status != kcall.StatusOK {
			sys.HandleClose(object)
			return supervisor.LaunchResourceExhausted, 0
		}
		launcher.object, launcher.memory = object, sys.Window(base, mapped)
	}
	path, ok := execproto.ResolveExecutable(command, &launcher.path)
	if !ok {
		return supervisor.LaunchNotFound, 0
	}
	builder := execproto.NewBuilder(launcher.memory[:execproto.MaxLaunchObject], cwd, environment, false)
	builder.Stage(path, argv)
	request, ok := builder.Finish(0)
	if !ok {
		return supervisor.LaunchArgumentsTooLong, 0
	}
	attached, status := sys.HandleDuplicate(launcher.object, capability.Read|capability.Transfer)
	if status != kcall.StatusOK {
		return supervisor.LaunchResourceExhausted, 0
	}
	launcher.outgoing, ok = execproto.EncodeLaunch(len(request), attached, correlation)
	if !ok || !launcher.send(&launcher.outgoing) {
		sys.HandleClose(attached)
		return supervisor.LaunchProcessFailed, 0
	}
	for {
		status, valid := launcher.receive(launchWait)
		if status == kcall.StatusTimeout {
			continue
		}
		if status != kcall.StatusOK {
			return supervisor.LaunchProcessFailed, 0
		}
		if !valid {
			continue
		}
		if endpoint, ok := tty.DecodeResizeEndpointNotice(&launcher.incoming); ok {
			replaceResize(endpoint)
			continue
		}
		if launcher.incoming.Correlation != correlation {
			continue
		}
		reply, valid := execproto.DecodeLaunchReply(launcher.incoming)
		if !valid {
			return supervisor.LaunchProcessFailed, 0
		}
		return supervisor.LaunchStatus(reply.LaunchStatus), reply.ExitCode
	}
}
