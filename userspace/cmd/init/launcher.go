//go:build goos_userspace

package main

// Application launch (ADR 0130). A foreground command whose manifest asks for
// GrantLaunch gets a channel to PID 1, and PID 1 keeps a copy of its standard
// input. Over the channel the command can run one full-screen command of
// /system/bin and wait for it. The child reads the caller's input and writes
// the terminal, while the caller waits and reads nothing, so one program at a
// time takes the keys. The caller passes no handles: the child's streams are
// the ones PID 1 kept, and its other authority comes from its own manifest.

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	userwait "goos/userspace/wait"
)

// launcher is what PID 1 keeps for a job that may launch a child.
type launcher struct {
	resize resizeDelivery
	// server is PID 1's end of the caller's launch channel.
	server handle.ID
	// input is PID 1's copy of the caller's standard input.
	input handle.ID
	// child is the running child, and correlation the request it answers.
	child       handle.ID
	correlation uint64
	// While the child's program is read from a file (ADR 0175), fill is the
	// read in flight and object and size the caller's request, which is
	// copied and checked again once the read is answered; path is the
	// program the read is for.
	fill     pendingFill
	object   handle.ID
	size     int
	path     [execproto.MaxPath]byte
	pathSize int
}

// launchData holds the request being handled, copied out of the caller's
// object so the caller cannot change it while PID 1 reads it.
var launchData [execproto.MaxLaunchObject]byte

// maxLaunchMapping bounds the object PID 1 maps to copy a request from.
const maxLaunchMapping = (execproto.MaxLaunchObject + 4095) &^ 4095

// interest is what the main loop waits on for the launcher: the child while
// one runs, the channel otherwise.
func (launch *launcher) interest() (userwait.Interest, bool) {
	switch {
	case launch.fill.session != 0:
		return userwait.Interest{Handle: launch.fill.session, Events: kcall.WaitReadable | kcall.WaitPeerClosed}, true
	case launch.child != 0:
		return userwait.Interest{Handle: launch.child, Events: kcall.WaitCompleted}, true
	case launch.server != 0:
		return userwait.Interest{Handle: launch.server, Events: kcall.WaitReadable | kcall.WaitPeerClosed}, true
	}
	return userwait.Interest{}, false
}

// pollLauncher reaps a job's finished child or takes its caller's next
// request.
func pollLauncher(job *execJob) {
	launch := &job.launcher
	if launch.fill.session != 0 {
		completeChildFill(job)
		return
	}
	if launch.child != 0 {
		code := waitApplicationExit(launch.child)
		launch.child = 0
		closeResize(&launch.resize)
		restoreParentResize(job)
		replyLaunch(launch, execproto.LaunchReply{ExitCode: code}, launch.correlation)
		return
	}
	if launch.server == 0 {
		return
	}
	arena.Reset(arenaBase)
	status := ipc.Status(sys.IPCReceive(launch.server, wire[:]))
	if status == ipc.StatusNotFound {
		return
	}
	if status != ipc.StatusOK {
		// The caller is gone; its job's end releases the rest.
		sys.HandleClose(launch.server)
		launch.server = 0
		return
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	message, ok := ipc.Decode(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8])
	if !ok {
		sys.HandleClose(launch.server)
		launch.server = 0
		return
	}
	size, valid := execproto.DecodeLaunch(message)
	for index := 0; index < int(message.HandleCount) && index < len(message.Handles); index++ {
		if index != 0 || !valid {
			sys.HandleClose(message.Handles[index])
		}
	}
	if !valid {
		replyLaunch(launch, execproto.LaunchReply{LaunchStatus: uint64(supervisor.LaunchInvalidImage)}, message.Correlation)
		return
	}
	request, copied := copyLaunchRequest(message.Handles[0], size)
	if !copied {
		sys.HandleClose(message.Handles[0])
		replyLaunch(launch, execproto.LaunchReply{LaunchStatus: uint64(supervisor.LaunchInvalidImage)}, message.Correlation)
		return
	}
	launch.correlation = message.Correlation
	command, launched := checkChild(launch, request)
	path := request.Stages[0].Path
	if launched == supervisor.LaunchOK && execproto.OriginOf(path) == execproto.OriginFile {
		// The program is read in the main loop, and the request copied and
		// checked again when it has been.
		pending, status := beginSource(path)
		if status == supervisor.LaunchOK {
			launch.fill, launch.object, launch.size = pending, message.Handles[0], size
			launch.pathSize = copy(launch.path[:], path)
			return
		}
		launched = status
	}
	sys.HandleClose(message.Handles[0])
	if launched == supervisor.LaunchOK {
		launched = startChild(job, request, command)
	}
	if launched != supervisor.LaunchOK {
		endChildLaunch(launch, launched)
	}
}

// checkChild decides whether a request may run, and what as.
func checkChild(launch *launcher, request execproto.Request) ([]byte, supervisor.LaunchStatus) {
	command, _, launched := supervisor.ChildLaunch(request, commandEntry)
	if launched == supervisor.LaunchOK && launch.input == 0 {
		launched = supervisor.LaunchResourceExhausted
	}
	return command, launched
}

// startChild launches a checked request on the job's terminal.
func startChild(job *execJob, request execproto.Request, command []byte) supervisor.LaunchStatus {
	launch := &job.launcher
	terminal := supervisor.Endpoint{ID: handle.ID(job.session.output[1]), Type: handle.TypeStream, Shared: true}
	streams := supervisor.Streams{Input: supervisor.Endpoint{ID: launch.input, Type: handle.TypeStream, Shared: true}, Output: terminal, Error: terminal}
	stage := request.Stages[0]
	var launched supervisor.LaunchStatus
	if job.resizeParent && !job.resizeReleased && job.session.resizeOwner.Handoff(uint64(job.slot)+1) {
		closeResize(&job.resize)
		launchingResize, launchingResizeSession = &launch.resize, job.session
	}
	_, launch.child, launched = launchApplication(stage.Path, command, request.CWD, stage.Arguments, request.Environment, streams, nil)
	launchingResize, launchingResizeSession = nil, nil
	if launched != supervisor.LaunchOK {
		restoreParentResize(job)
	}
	discardReady()
	if launched == supervisor.LaunchOK {
		syslog.Info("application launched", syslog.Str("command", bytesString(command)))
	}
	return launched
}

// completeChildFill takes the VFS's answer to the read of a child's program.
// The request is copied again from the caller's object, which it could have
// changed meanwhile, checked again, and launched only if it still names the
// program that was read.
func completeChildFill(job *execJob) {
	launch := &job.launcher
	pending := launch.fill
	launch.fill = pendingFill{}
	message, status := receiveNow(pending.session)
	executable, launched := finishSource(pending, message, status)
	request, copied := copyLaunchRequest(launch.object, launch.size)
	sys.HandleClose(launch.object)
	launch.object = 0
	var command []byte
	if launched == supervisor.LaunchOK {
		command, launched = checkChild(launch, request)
	}
	if launched == supervisor.LaunchOK && (!copied || !equalPath(request.Stages[0].Path, launch.path[:launch.pathSize])) {
		launched = supervisor.LaunchInvalidImage
	}
	if launched == supervisor.LaunchOK {
		setReady(launch.path[:launch.pathSize], executable, launched)
		launched = startChild(job, request, command)
	} else {
		closeExecutable(executable)
	}
	if launched != supervisor.LaunchOK {
		endChildLaunch(launch, launched)
	}
}

// endChildLaunch answers a launch that started no child.
func endChildLaunch(launch *launcher, launched supervisor.LaunchStatus) {
	if launch.object != 0 {
		sys.HandleClose(launch.object)
		launch.object = 0
	}
	launch.child = 0
	replyLaunch(launch, execproto.LaunchReply{LaunchStatus: uint64(launched)}, launch.correlation)
}

// copyLaunchRequest copies size bytes from the start of object and decodes
// them. An object larger than a request can be is refused.
func copyLaunchRequest(object handle.ID, size int) (execproto.Request, bool) {
	base, mapped, status := sys.MemoryMapAnywhere(object, false)
	if status != kcall.StatusOK {
		return execproto.Request{}, false
	}
	ok := mapped <= maxLaunchMapping && uint64(size) <= mapped
	if ok {
		copy(launchData[:size], sys.Window(base, uint64(size)))
	}
	sys.VMUnmap(base, mapped)
	if !ok {
		return execproto.Request{}, false
	}
	return execproto.DecodeRequest(launchData[:size])
}

func replyLaunch(launch *launcher, reply execproto.LaunchReply, correlation uint64) {
	message, ok := execproto.EncodeLaunchReply(reply, correlation)
	if ok && launch.server != 0 && !sendMessage(uint64(launch.server), message) {
		syslog.Warn("launch reply undelivered")
	}
}

// stopChild stops a job's running child, as its caller is stopped.
func stopChild(launch *launcher) {
	if launch.child != 0 {
		sys.ProcessKill(launch.child)
	}
}

// closeLauncher ends a job's launcher when the job ends: a child still
// running is stopped and reaped first, so it cannot outlive the job and read
// input the shell no longer feeds it raw.
func closeLauncher(launch *launcher) {
	closeResize(&launch.resize)
	if launch.fill.session != 0 {
		dropFill(&launch.fill)
	}
	if launch.object != 0 {
		sys.HandleClose(launch.object)
	}
	if launch.child != 0 {
		sys.ProcessKill(launch.child)
		waitApplicationExit(launch.child)
	}
	if launch.server != 0 {
		sys.HandleClose(launch.server)
	}
	if launch.input != 0 {
		sys.HandleClose(launch.input)
	}
	*launch = launcher{}
}

// commandEntry returns the grants of the command at path from the index that
// describes it, and whether one does.
func commandEntry(path []byte) (commands.Grant, bool) {
	return trustedGrants(path, execproto.OriginOf(path) == execproto.OriginArchive)
}
