//go:build goos_userspace

package main

// Restarting the block service (ADR 0101). Its channels to the driver and to
// the filesystem service die with it, and so do those two services' ends: both
// exit when they see it gone, and the supervisor starts them again after it,
// each handing the new block service its endpoint over the control channel as
// after any restart of either. The block service itself is launched with a
// new control channel and a new public endpoint, and without a driver or a
// filesystem endpoint.

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

var (
	// blockStarted marks the first launch, which uses the channels created at
	// boot; every later launch renews them.
	blockStarted bool
	// blockClient is PID 1's end of the block service's public endpoint after
	// a restart. Nothing uses it yet; it is kept so the endpoint has a holder.
	blockClient handle.ID
	// blockProcessHandle is kept only by the validation build, to inject the
	// fault that proves a crashed block service is replaced.
	blockProcessHandle handle.ID
)

// renewBlockGrants prepares a restarting block service.
func renewBlockGrants(spec *supervisor.LaunchSpec) bool {
	var control, public [2]uint64
	if !createChannel(&control) {
		syslog.Error("block control channel unavailable")
		return false
	}
	if !createChannel(&public) {
		closePair(control)
		syslog.Error("block endpoint unavailable")
		return false
	}
	if !renewGrant(spec, "block", "control", handle.ID(control[1])) || !renewGrant(spec, "block", "block", handle.ID(public[1])) {
		closePair(control)
		closePair(public)
		return false
	}
	// The driver and the filesystem service bring their endpoints when they
	// start again; the ones granted at boot died with the old instance.
	for _, name := range [...]string{"driver", "filesystem"} {
		manager.RemoveCapability("block", name)
		supervisor.RemoveGrant(spec, name)
	}
	sys.HandleClose(handle.ID(blockControl[0]))
	blockControl[0] = control[0]
	if blockClient != 0 {
		sys.HandleClose(blockClient)
	}
	blockClient = handle.ID(public[0])
	return true
}

// injectBlockFault kills the block service, in the validation build only, so
// the boot test sees it restarted under a running system.
func injectBlockFault() bool {
	if !validation.Enabled || blockProcessHandle == 0 || sys.ProcessKill(blockProcessHandle) != 0 {
		return false
	}
	sys.HandleClose(blockProcessHandle)
	blockProcessHandle = 0
	syslog.Warn("block fault injected")
	mark("BLOCK_FAULT_INJECTED\n")
	return true
}
