//go:build goos_userspace

package main

// Restarting the VFS (ADR 0099). The service is restarted on failure like the
// others, but everything that talked to it held a channel that died with it:
// the control endpoint PID 1 uses, the shell's session, the config and log
// services' sessions, and the channel to the filesystem service. The first two
// are renewed for the launch, the sessions are handed to their clients over
// channels those clients already hold with PID 1, and the filesystem service
// restarts itself and is attached as after any restart.

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/vfs"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

var (
	// vfsStarted marks the first launch, which uses the channels the kernel
	// created at boot; every later launch renews them.
	vfsStarted bool
	// shellFiles is the client end of the session a restarted VFS serves
	// first, held until the launch succeeds and then handed to the shell.
	shellFiles handle.ID
	// vfsClientsPending records a restart whose config and log clients have
	// not been handed a new session yet. They get it once the filesystem
	// service is attached again, so that what they write reaches the volume.
	vfsClientsPending bool
	// logControl is PID 1's end of the log service's control channel.
	logControl handle.ID
	// vfsProcessHandle is kept only by the validation build, to inject the
	// fault that proves a crashed VFS is replaced.
	vfsProcessHandle handle.ID
)

// renewVFSChannels gives a restarting VFS a new control endpoint, whose client
// end PID 1 keeps, and a new first session, whose client end goes to the
// shell once the service runs.
func renewVFSChannels(spec *supervisor.LaunchSpec) bool {
	var control, session [2]uint64
	if !createChannel(&control) {
		syslog.Error("vfs control channel unavailable")
		return false
	}
	if !createChannel(&session) {
		closePair(control)
		syslog.Error("vfs session unavailable")
		return false
	}
	if !renewGrant(spec, "vfs", "control", handle.ID(control[1])) || !renewGrant(spec, "vfs", "session", handle.ID(session[1])) {
		closePair(control)
		closePair(session)
		return false
	}
	sys.HandleClose(handle.ID(vfsControlHandle))
	vfsControlHandle = control[0]
	if shellFiles != 0 {
		sys.HandleClose(shellFiles)
	}
	shellFiles = handle.ID(session[0])
	return true
}

// vfsRestarted defers user sessions until the filesystem is attached and
// the private host-key scopes have been restored.
func vfsRestarted() {
	closeOpen(shellFiles)
	shellFiles = 0
	remoteScopesInstalled = false
	for index := range remoteScopes {
		remoteScopes[index].installed = false
	}
	vfsClientsPending = true
	syslog.Info("vfs restarted")
	mark("VFS_RESTARTED_OK\n")
}

// renewVFSClients hands the config and log services new sessions after a VFS
// restart, once the filesystem service has been attached or has failed to be.
func renewVFSClients() {
	if !vfsClientsPending {
		return
	}
	if boot.Mode() == supervisor.ModeNormal && !protectRemoteStorage() {
		fatal("INIT_REMOTE_STORAGE_PROTECTION_FAILED\n")
	}
	vfsClientsPending = false
	if session, reason := openVFSSession(); reason == "" {
		offerFiles(sessions[0].execServer, session)
	}
	for _, channel := range append(otherShells(), uint64(configControlHandle), uint64(logControl)) {
		if channel == 0 {
			continue
		}
		session, reason := openVFSSession()
		if reason != "" {
			syslog.Warn("service not given a filesystem session", syslog.Str("reason", reason))
			continue
		}
		offerFiles(channel, session)
	}
}

// refreshMountedClients replaces sessions whose handles into the persistent
// mount were invalidated by an intentional unmount. The VFS process itself
// survives, so this is separate from the VFS-restart path above.
func refreshMountedClients() {
	for _, channel := range append(otherShells(), sessions[0].execServer, uint64(configControlHandle), uint64(logControl)) {
		if channel == 0 {
			continue
		}
		session, reason := openVFSSession()
		if reason != "" {
			syslog.Warn("client not given a refreshed filesystem session", syslog.Str("reason", reason))
			continue
		}
		offerFiles(channel, session)
	}
}

// offerFiles hands one filesystem session to a client over a channel it
// holds with PID 1. A client that is gone gets nothing, and the session is
// closed.
func offerFiles(channel uint64, session handle.ID) bool {
	if channel != 0 && session != 0 {
		if notice, ok := vfs.SessionNotice(session); ok && sendMessage(channel, notice) {
			return true
		}
	}
	if session != 0 {
		sys.HandleClose(session)
	}
	return false
}

// injectVFSFault kills the VFS, in the validation build only, so the boot test
// sees it restarted under a running system.
func injectVFSFault() bool {
	if !validation.Enabled || vfsProcessHandle == 0 || sys.ProcessKill(vfsProcessHandle) != 0 {
		return false
	}
	sys.HandleClose(vfsProcessHandle)
	vfsProcessHandle = 0
	syslog.Warn("vfs fault injected")
	mark("VFS_FAULT_INJECTED\n")
	return true
}

// otherShells returns the exec channels of the shells after the first, which
// are handed a session opened for them rather than the one a restarted VFS
// starts with.
func otherShells() []uint64 {
	var channels []uint64
	for index := 1; index < len(sessions); index++ {
		if sessions[index].active {
			channels = append(channels, sessions[index].execServer)
		}
	}
	return channels
}
