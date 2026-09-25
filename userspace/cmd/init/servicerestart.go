//go:build goos_userspace

package main

// Restarting the configuration and log services on their own. Their first
// launch consumes the sessions PID 1 opened for them, so a restart needs new
// ones; and the sessions other processes held with the configuration service
// die with it, so its clients are handed new ones over the channels they
// already hold with PID 1 (ADR 0100).

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	configproto "goos/userspace/config"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

var (
	// configStarted and logStarted mark the first launch of each; every later
	// launch renews what the first consumed.
	configStarted bool
	logStarted    bool
	// configProcessHandle and logProcessHandle are kept only by the
	// validation build, to inject the faults that prove each is replaced.
	configProcessHandle handle.ID
	logProcessHandle    handle.ID
)

// logConfigScope and logConfigRights are what the log service may see of the
// configuration: its own namespace, read-only.
const logConfigScope = "system.log."

// renewLogGrants gives a restarting log service a new filesystem session, a
// new configuration session if it had one, and a new control channel, whose
// other end PID 1 keeps.
func renewLogGrants(spec *supervisor.LaunchSpec) bool {
	files, reason := openVFSSession()
	if reason != "" {
		syslog.Error("log filesystem session unavailable", syslog.Str("reason", reason))
		return false
	}
	if !renewGrant(spec, "log", "vfs", files) {
		sys.HandleClose(files)
		return false
	}
	var control [2]uint64
	if !createChannel(&control) || !renewGrant(spec, "log", "control", handle.ID(control[1])) {
		closePair(control)
		return false
	}
	if logControl != 0 {
		sys.HandleClose(logControl)
	}
	logControl = handle.ID(control[0])
	renewOptionalConfig(spec, "log", logConfigScope, configproto.Read)
	return true
}

// renewConfigGrants gives a restarting configuration service a new filesystem
// session and a new control channel. The control channel's other end is the
// one PID 1 opens every configuration session over.
func renewConfigGrants(spec *supervisor.LaunchSpec) bool {
	files, reason := openVFSSession()
	if reason != "" {
		syslog.Error("config filesystem session unavailable", syslog.Str("reason", reason))
		return false
	}
	if !renewGrant(spec, "config", "vfs", files) {
		sys.HandleClose(files)
		return false
	}
	var control [2]uint64
	if !createChannel(&control) || !renewGrant(spec, "config", "control", handle.ID(control[1])) {
		closePair(control)
		return false
	}
	if configControlHandle != 0 {
		sys.HandleClose(configControlHandle)
	}
	configControlHandle = handle.ID(control[0])
	return true
}

// configRestarted runs once a restarted configuration service is up. The
// sessions the shell and the log service held died with the old instance;
// each is handed a new one, limited as before, over its channel with PID 1.
func configRestarted() {
	offerSettings(execServer, "", configproto.Read|configproto.Write)
	offerSettings(uint64(logControl), logConfigScope, configproto.Read)
	syslog.Info("config restarted")
	mark("CONFIG_RESTARTED_OK\n")
}

func offerSettings(channel uint64, scope string, rights configproto.Rights) {
	if channel == 0 {
		return
	}
	session, ok := openConfigSession(scope, rights)
	if !ok {
		syslog.Warn("configuration session not renewed", syslog.Str("scope", scope))
		return
	}
	if notice, encoded := configproto.SessionNotice(session); !encoded || !sendMessage(channel, notice) {
		sys.HandleClose(session)
	}
}

// renewOptionalConfig renews a configuration session a service was launched
// with, or drops the grant when none can be opened: configuration is optional
// for the services that read it.
func renewOptionalConfig(spec *supervisor.LaunchSpec, name, scope string, rights configproto.Rights) {
	if _, granted := manager.Capability(name, "config"); !granted {
		return
	}
	if settings, ok := openConfigSession(scope, rights); ok && renewGrant(spec, name, "config", settings) {
		return
	} else if ok {
		sys.HandleClose(settings)
	}
	if previous, found := manager.Capability(name, "config"); found {
		sys.HandleClose(previous)
	}
	manager.RemoveCapability(name, "config")
	supervisor.RemoveGrant(spec, "config")
}

// injectServiceFault kills the configuration or log service, in the
// validation build only, so the boot test sees each replaced.
func injectServiceFault(process *handle.ID, marker string) bool {
	if !validation.Enabled || *process == 0 || sys.ProcessKill(*process) != 0 {
		return false
	}
	sys.HandleClose(*process)
	*process = 0
	syslog.Warn("service fault injected")
	mark(marker)
	return true
}
