//go:build goos_userspace

package main

// Launching a service from its declaration: the startup template, the launch
// request, and the handles PID 1 keeps or renews for it.

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
)

func bootstrapService(supervisor.LaunchSpec) (uint64, bool) { return 3, true }

// stagingSlot returns the next slot of the area shared with the kernel for
// launch startup templates.
func stagingSlot() (uint8, []byte) {
	nextStagingSlot = (nextStagingSlot + 1) % supervisor.StagingSlots
	address := supervisor.StagingAddress + uint64(nextStagingSlot)*supervisor.StagingSlotSize
	return nextStagingSlot, sys.MappedAtStart(address, supervisor.StagingSlotSize)
}

func launchService(spec supervisor.LaunchSpec) (uint64, bool) {
	// A driver that is starting again needs a channel whose other end is live.
	// The first launch uses the pair created at boot, which still is.
	if driverStarted && bytesString(spec.Name[:spec.NameSize]) == driverName && !replaceDriverChannel(&spec) {
		return 0, false
	}
	// So do a terminal service and a shell, and so does the peer of the same
	// session that survived.
	if session, tty, found := sessionOfService(bytesString(spec.Name[:spec.NameSize])); found {
		if tty && session.ttyStarted && !renewTerminalSession(session, &spec) ||
			!tty && session.shellStarted && !renewShellSession(session, &spec) {
			return 0, false
		}
	}
	if bytesString(spec.Name[:spec.NameSize]) == filesystemName && (storageLost || filesystemStarted && !renewFilesystemChannels(&spec)) {
		return 0, false
	}
	if blockStarted && bytesString(spec.Name[:spec.NameSize]) == "block" && !renewBlockGrants(&spec) {
		return 0, false
	}
	if vfsStarted && bytesString(spec.Name[:spec.NameSize]) == "vfs" && !renewVFSChannels(&spec) {
		return 0, false
	}
	if configStarted && bytesString(spec.Name[:spec.NameSize]) == "config" && !renewConfigGrants(&spec) {
		return 0, false
	}
	if logStarted && bytesString(spec.Name[:spec.NameSize]) == "log" && !renewLogGrants(&spec) {
		return 0, false
	}
	// A service of the installed system is read from the volume; every other
	// one, and every service in recovery, comes from the archive (ADR 0175).
	executable, processName, located := openExecutable(spec.Executable[:spec.ExecutableSize])
	if located != supervisor.LaunchOK {
		if !supervisionComplete {
			write("[FAIL] " + bytesString(spec.Name[:spec.NameSize]) + "\n")
		}
		syslog.Error("service executable not found", syslog.Str("service", bytesString(spec.Name[:spec.NameSize])), syslog.Uint("status", uint64(located)))
		return 0, false
	}
	slot, staging := stagingSlot()
	// A service started again in this boot is told so. What it left behind,
	// such as the end of the log file, is then its own work to pick up
	// rather than an earlier boot's (ADR 0100).
	restarted := false
	if snapshot, found := manager.Snapshot(bytesString(spec.Name[:spec.NameSize])); found && snapshot.Restarts != 0 {
		restarted = true
	}
	_, tty, interactive := sessionOfService(bytesString(spec.Name[:spec.NameSize]))
	// A shell searches the commands of the mode it was started in.
	shell := interactive && !tty
	variables := 0
	if restarted {
		variables++
	}
	if shell {
		variables++
	}
	writer := startup.NewWriter(staging, 1, variables, int(spec.CapabilityCount))
	writer.Command(spec.Name[:spec.NameSize])
	writer.CWD([]byte("/"))
	writer.Argument(spec.Name[:spec.NameSize])
	if restarted {
		writer.Variable([]byte(startup.RestartVariable), []byte("1"))
	}
	if shell {
		writer.Variable([]byte("PATH"), []byte(supervisor.SearchPath(boot.Mode())))
	}
	for index := 0; index < int(spec.CapabilityCount); index++ {
		writer.Handle([]byte(spec.Capabilities[index].Name), 0)
	}
	startupData, ok := writer.Finish()
	if !ok {
		closeExecutable(executable)
		return 0, false
	}
	event, launched := requestLaunch(executable, processName, spec.Capabilities[:spec.CapabilityCount], spec.Profile, slot, uint32(len(startupData)))
	// The process holds its own reference to a file-backed source.
	closeExecutable(executable)
	mark("INIT_SERVICE_LAUNCH_OK\n")
	name := bytesString(spec.Name[:spec.NameSize])
	// The validation build keeps the storage services' process handles so it
	// can inject the faults that prove each is replaced. The services that
	// run from the volume are kept in every build: leaving normal mode stops
	// them. Every other handle is closed: PID 1 supervises through lifecycle
	// events, not handles.
	session, tty, interactive := sessionOfService(name)
	keep := validation.Enabled && (name == driverName && driverProcessHandle == 0 || interactive || name == filesystemName || name == "vfs" || name == "block") ||
		diskService(name)
	if keep && launched == supervisor.LaunchOK {
		switch {
		case interactive && tty:
			session.ttyProcess = keepProcess(session.ttyProcess, event.ProcessHandle)
		case interactive:
			session.shellProcess = keepProcess(session.shellProcess, event.ProcessHandle)
		}
		switch name {
		case driverName:
			driverProcessHandle = event.ProcessHandle
		case filesystemName:
			filesystemProcessHandle = keepProcess(filesystemProcessHandle, event.ProcessHandle)
		case "vfs":
			vfsProcessHandle = keepProcess(vfsProcessHandle, event.ProcessHandle)
		case "config":
			configProcessHandle = keepProcess(configProcessHandle, event.ProcessHandle)
		case "log":
			logProcessHandle = keepProcess(logProcessHandle, event.ProcessHandle)
		case "block":
			blockProcessHandle = keepProcess(blockProcessHandle, event.ProcessHandle)
		}
	}
	if launched != supervisor.LaunchOK || !keep && sys.HandleClose(event.ProcessHandle) != 0 {
		if !supervisionComplete {
			write("[FAIL] " + name + "\n")
		}
		syslog.Error("service launch failed", syslog.Str("service", name), syslog.Uint("status", uint64(launched)))
		return 0, false
	}
	mark("INIT_SERVICE_STARTED_OK\n")
	if execproto.OriginOf(spec.Executable[:spec.ExecutableSize]) == execproto.OriginFile {
		mark("SERVICE_FROM_DISK_OK " + name + "\n")
	}
	// Once the prompt is up the console belongs to the terminal service, and
	// a line written past it would land in whatever is being typed. A restart
	// after boot is recorded in the log instead.
	if !supervisionComplete {
		write("[ OK ] " + name + "\n")
	}
	syslog.Info("service started", syslog.Str("service", name), syslog.Uint("pid", event.Process))
	if name == driverName {
		if driverStarted {
			driverRestarted()
		}
		driverStarted = true
	}
	if name == "block" {
		if blockStarted {
			syslog.Info("block restarted")
			mark("BLOCK_RESTARTED_OK\n")
		}
		blockStarted = true
	}
	if name == "vfs" {
		if vfsStarted {
			vfsRestarted()
		}
		vfsStarted = true
	}
	if name == "config" {
		if configStarted {
			configRestarted()
		}
		configStarted = true
	}
	if name == "log" {
		if logStarted {
			mark("LOG_RESTARTED_OK\n")
		}
		logStarted = true
	}
	if name == filesystemName {
		restarted := filesystemStarted
		attachFilesystem()
		filesystemStarted = true
		if restarted {
			restoreSystem()
		}
		renewVFSClients()
	}
	if interactive && tty {
		if session.ttyStarted {
			mark("TTY_RESTARTED_OK\n")
		}
		session.ttyStarted = true
	}
	if interactive && !tty {
		if session.shellStarted {
			mark("SHELL_RESTARTED_OK\n")
		}
		session.shellStarted = true
	}
	return event.Process, true
}

// keepProcess replaces a kept process handle; the one it replaces names a
// process that has exited.
func keepProcess(previous, next handle.ID) handle.ID {
	if previous != 0 {
		sys.HandleClose(previous)
	}
	return next
}

// renewGrant points one capability of a service at a fresh handle, in both the
// definition and the launch about to happen. A handle the definition named and
// no launch consumed — one renewed for a launch that then failed — is closed
// rather than left in PID 1's table; one that was consumed is already gone, and
// closing its stale ID is refused.
func renewGrant(spec *supervisor.LaunchSpec, name string, capabilityName string, id handle.ID) bool {
	if previous, found := manager.Capability(name, capabilityName); found && previous != id {
		sys.HandleClose(previous)
	}
	if manager.SetCapability(name, capabilityName, id) != supervisor.StatusOK {
		return false
	}
	for index := 0; index < int(spec.CapabilityCount); index++ {
		if spec.Capabilities[index].Name == capabilityName {
			spec.Capabilities[index].Handle = id
			return true
		}
	}
	return false
}
