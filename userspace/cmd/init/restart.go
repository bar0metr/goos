//go:build goos_userspace

package main

// Restart supervision: the channels a restarted driver, terminal, or shell is
// given, and the faults the validation build injects to exercise them
// (ADR 0078, 0089).

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	blockproto "goos/userspace/block"
	configproto "goos/userspace/config"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
	terminal "goos/userspace/tty"
)

// injectDriverFault kills the storage driver once, in the validation build
// only, so that the path a crashed driver takes is exercised on every boot
// rather than only when hardware misbehaves. The supervisor restarts it, PID 1
// gives it a fresh channel, and the block service installs it.
func injectDriverFault() {
	if !validation.Enabled || driverProcessHandle == 0 {
		return
	}
	target := driverProcessHandle
	driverProcessHandle = 0
	if sys.ProcessKill(target) != 0 {
		syslog.Warn("driver fault injection failed")
		return
	}
	sys.HandleClose(target)
	syslog.Warn("driver fault injected")
	mark("BLOCK_DRIVER_FAULT_INJECTED\n")
}

// replaceDriverChannel gives a restarting driver a fresh channel to the block
// service. The old pair died with the process that held one half of it, so the
// replacement cannot be handed the endpoint its predecessor had: a new pair is
// created, the block service is told about the client side over its control
// endpoint, and the definition is updated so the next restart does not hand out
// a handle that is already dead.
func replaceDriverChannel(spec *supervisor.LaunchSpec) bool {
	var pair [2]uint64
	if !createChannel(&pair) {
		syslog.Error("driver channel unavailable")
		return false
	}
	correlation++
	request, ok := blockproto.SetDriverRequest(handle.ID(pair[0]), correlation)
	if !ok || !sendMessage(uint64(blockControl[0]), request) {
		closePair(pair)
		return false
	}
	reply, status := receive(uint64(blockControl[0]), vfsSessionDeadline)
	blockStatus, valid := blockproto.DecodeSetDriverReply(reply)
	if status != ipc.StatusOK || !valid || blockStatus != blockproto.StatusOK {
		sys.HandleClose(handle.ID(pair[1]))
		syslog.Error("block service refused the replacement driver")
		return false
	}
	// The definition and the copy about to be launched both have to name the
	// new endpoint: the launcher was handed its copy before this ran, and the
	// definition is what the launch after this one will use.
	if manager.SetCapability(driverName, "service", handle.ID(pair[1])) != supervisor.StatusOK {
		sys.HandleClose(handle.ID(pair[1]))
		return false
	}
	for index := 0; index < int(spec.CapabilityCount); index++ {
		if spec.Capabilities[index].Name == "service" {
			spec.Capabilities[index].Handle = handle.ID(pair[1])
			return true
		}
	}
	sys.HandleClose(handle.ID(pair[1]))
	return false
}

// offerSession hands one end of a new session to the peer that survived, as a
// typed notice over channel. A peer that is gone too gets nothing: the end is
// closed, and the peer's own restart brings it a session of its own.
func offerSession(channel uint64, session handle.ID) bool {
	if channel != 0 {
		if notice, ok := terminal.SessionNotice(session); ok && sendMessage(channel, notice) {
			return true
		}
	}
	sys.HandleClose(session)
	return false
}

// renewTerminalSession prepares a restarting terminal service (ADR 0089). Its
// session and control channels died with it: it gets new ones, and the running
// shell is given the client end of the session over its exec channel. The
// output stream needs nothing: PID 1 holds both of its ends, so what was
// written while the terminal was down is waiting in it.
func renewTerminalSession(session *terminalSession, spec *supervisor.LaunchSpec) bool {
	var pair, control [2]uint64
	if !createChannel(&pair) {
		syslog.Error("terminal session unavailable")
		return false
	}
	if !createChannel(&control) {
		closePair(pair)
		syslog.Error("terminal control unavailable")
		return false
	}
	if !renewGrant(spec, session.tty, "session", handle.ID(pair[1])) || !renewGrant(spec, session.tty, "control", handle.ID(control[1])) {
		closePair(pair)
		closePair(control)
		return false
	}
	if session.ttyControl != 0 {
		sys.HandleClose(session.ttyControl)
	}
	session.ttyControl = handle.ID(control[0])
	if offerSession(session.execServer, handle.ID(pair[0])) {
		mark("TTY_SESSION_OFFERED_OK\n")
	}
	return true
}

// renewShellSession prepares a restarting shell (ADR 0089). Every channel the
// shell held died with it: it gets a new terminal session, whose server end is
// handed to the running terminal service, a new exec channel, whose server end
// PID 1 keeps, and new filesystem and configuration sessions.
func renewShellSession(terminal *terminalSession, spec *supervisor.LaunchSpec) bool {
	var session, exec [2]uint64
	if !createChannel(&session) {
		syslog.Error("shell session unavailable")
		return false
	}
	if !createChannel(&exec) {
		closePair(session)
		syslog.Error("shell exec channel unavailable")
		return false
	}
	files, reason := openVFSSession()
	if reason != "" {
		closePair(session)
		closePair(exec)
		syslog.Error("shell filesystem session unavailable", syslog.Str("reason", reason))
		return false
	}
	if !renewGrant(spec, terminal.shell, "tty", handle.ID(session[0])) || !renewGrant(spec, terminal.shell, "exec", handle.ID(exec[0])) ||
		!renewGrant(spec, terminal.shell, "vfs", files) {
		closePair(session)
		closePair(exec)
		sys.HandleClose(files)
		return false
	}
	// Configuration is optional, as it is at boot: a shell without a session
	// keeps its built-in prompt rather than failing to start.
	if _, granted := manager.Capability(terminal.shell, "config"); granted {
		settings, ok := openConfigSession("", configproto.Read|configproto.Write)
		if !ok || !renewGrant(spec, terminal.shell, "config", settings) {
			if ok {
				sys.HandleClose(settings)
			}
			if previous, found := manager.Capability(terminal.shell, "config"); found {
				sys.HandleClose(previous)
			}
			manager.RemoveCapability(terminal.shell, "config")
			supervisor.RemoveGrant(spec, "config")
		}
	}
	if terminal.execServer != 0 {
		sys.HandleClose(handle.ID(terminal.execServer))
	}
	terminal.execServer = exec[1]
	if offerSession(uint64(terminal.ttyControl), handle.ID(session[1])) {
		mark("SHELL_SESSION_OFFERED_OK\n")
	}
	return true
}

// endShellSession runs when the shell exits. Its jobs belong to its session:
// every stage still running is stopped, and each job is reaped as usual but
// answered to no one. Their streams close with them — a job's input writer was
// the shell's, and its pipes and its copies of the terminal's writing end go
// with each stage's teardown — so nothing is left behind (ADR 0089). The exec
// channel's other end is gone, so it is closed and not waited on until a new
// shell brings a new one.
func endShellSession(session *terminalSession) {
	stopped := false
	for index := range execJobs {
		job := &execJobs[index]
		if !job.active || job.session != session {
			continue
		}
		job.orphaned = true
		if cancelPreparation(job) {
			continue
		}
		for _, process := range job.job.Processes() {
			if process != 0 && sys.ProcessKill(process) == 0 {
				stopped = true
			}
		}
	}
	if stopped {
		syslog.Info("session jobs stopped")
		mark("SESSION_JOBS_STOPPED_OK\n")
	}
	if session.execServer != 0 {
		sys.HandleClose(handle.ID(session.execServer))
		session.execServer = 0
	}
}

// injectSessionFault kills the terminal service or the shell of the session
// that asked, in the validation build only, so that each is seen to restart on
// its own while the other keeps working (ADR 0089).
func injectSessionFault(session *terminalSession, target uint8) execproto.ControlStatus {
	if !validation.Enabled {
		return execproto.ControlInvalid
	}
	if target == execproto.FaultKernelStack {
		// A successful call does not return: the kernel reports the double
		// fault and halts.
		mark("KERNEL_STACK_FAULT_INJECTED\n")
		sys.ValidationFault(handle.ID(systemControlHandle))
		return execproto.ControlInvalid
	}
	process := &session.ttyProcess
	name := "TTY_FAULT_INJECTED\n"
	if target == execproto.FaultShell {
		process, name = &session.shellProcess, "SHELL_FAULT_INJECTED\n"
	}
	if *process == 0 || sys.ProcessKill(*process) != 0 {
		return execproto.ControlNotFound
	}
	sys.HandleClose(*process)
	*process = 0
	syslog.Warn("session fault injected")
	mark(name)
	return execproto.ControlOK
}
