//go:build goos_userspace

package main

// The identity service (ADR 0233). PID 1 runs identityd from the installed
// volume in normal mode, after the persistent volume is mounted, with a
// control channel whose client end PID 1 keeps and a VFS session confined to
// /state/identity. Commands that declare GrantIdentityAdmin are handed an
// administration session over that control channel.

import (
	"goos/internal/capability"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/identityproto"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
)

const (
	identityServiceName = "identityd"
	// identityDirectory is the one path identityd may touch: its snapshot
	// file and any safesave temp file live under it (ADR 0233).
	identityDirectory = "/state/identity"
	// identityScopeSlot is the single scope slot identityd's VFS session
	// grants. Scope slots are never recycled within a namespace.
	identityScopeSlot = 0
	// identityControlDeadline bounds the wait for identityd's control reply.
	identityControlDeadline = 2 * clock.TicksPerSecond
)

var (
	// identityControl is PID 1's end of identityd's control channel.
	identityControl handle.ID
	// identityProcess is kept so recovery can stop the service.
	identityProcess handle.ID
	// identityScopeInstalled says the /state/identity subtree scope is bound
	// in the current namespace. A VFS restart clears it, as it does the
	// remoted scopes.
	identityScopeInstalled bool
)

// startIdentity registers and starts identityd. It runs only in normal mode,
// from the volume, once /state is in place.
func startIdentity(ticks uint64) {
	if boot.Mode() != supervisor.ModeNormal {
		return
	}
	if manager.Register(supervisor.Definition{Name: identityServiceName, Executable: supervisor.ServicePath(identityServiceName, boot.Mode()),
		Dependencies: []string{"vfs"},
		Capabilities: []supervisor.Capability{
			{Name: "log", Handle: logDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
		}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		syslog.Warn("identity service registration failed")
		return
	}
	manager.StartReady(ticks+supervisor.RestartBackoff, launchService)
}

// prepareIdentityGrants gives a launching identityd fresh consumed endpoints.
// It runs before every launch, the first included, so a restarted identityd
// never keeps the previous instance's channel or session.
func prepareIdentityGrants(spec *supervisor.LaunchSpec) bool {
	closeOpen(identityControl)
	identityControl = 0
	return renewIdentityGrants(spec)
}

// renewIdentityGrants creates the fresh control channel and confined VFS
// session of one launch and installs them in the spec.
func renewIdentityGrants(spec *supervisor.LaunchSpec) bool {
	var control [2]uint64
	if !createChannel(&control) {
		return false
	}
	files, reason := openIdentityVFS()
	if reason != "" {
		closePair(control)
		return false
	}
	grants := []supervisor.Capability{
		{Name: "control", Handle: handle.ID(control[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "vfs", Handle: files, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
	}
	for _, grant := range grants {
		spec.Capabilities[spec.CapabilityCount] = grant
		spec.CapabilityCount++
	}
	identityControl = handle.ID(control[0])
	return true
}

// abortIdentityGrants closes the consumed endpoints of a launch that failed.
func abortIdentityGrants(spec *supervisor.LaunchSpec) {
	for index := 1; index < int(spec.CapabilityCount); index++ {
		closeOpen(spec.Capabilities[index].Handle)
	}
	closeOpen(identityControl)
	identityControl = 0
}

// openIdentityVFS installs the /state/identity scope if the namespace does not
// have it yet, then opens the confined session identityd runs against.
func openIdentityVFS() (handle.ID, string) {
	if !ensureIdentityScope() {
		return 0, "IDENTITY_SCOPE_INSTALL_FAILED"
	}
	authority := vfs.Authority{Confined: true}
	authority.Grants[identityScopeSlot] = vfs.Read | vfs.Write
	return openIdentityAuthoritySession(authority)
}

// ensureIdentityScope creates /state/identity and binds scope slot
// identityScopeSlot to it as a subtree denied Read|Write, once per namespace.
func ensureIdentityScope() bool {
	if identityScopeInstalled {
		return true
	}
	files, reason := openVFSSession()
	if reason != "" {
		return false
	}
	defer closeOpen(files)
	if !ensureIdentityDirectory(files) {
		return false
	}
	correlation++
	request, ok := vfs.InstallScopeRequest(identityScopeSlot, []byte(identityDirectory), true, vfs.Read|vfs.Write, 0, correlation)
	if !ok {
		return false
	}
	result, status, _ := filesystemRequests.Control(handle.ID(vfsControlHandle), &request, vfsSessionDeadline)
	identityScopeInstalled = status == ipc.StatusOK && result == vfs.StatusOK
	return identityScopeInstalled
}

// ensureIdentityDirectory creates /state and /state/identity when they are
// missing. /state is the persistent volume's mount, already present in normal
// mode; identityd's own directory is made through PID 1's unconfined session.
func ensureIdentityDirectory(files handle.ID) bool {
	for _, prefix := range [...]string{"/state", identityDirectory} {
		stat, ok := callSession(files, vfs.Request{Operation: vfs.OperationStat, Path: []byte(prefix)})
		if ok && stat.Status == vfs.StatusOK && stat.Kind == ramfs.KindDirectory {
			continue
		}
		reply, ok := callSession(files, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte(prefix)})
		if !ok || reply.Status != vfs.StatusOK {
			return false
		}
	}
	return true
}

// openIdentityAuthoritySession asks the VFS for a session with authority, the
// same handshake the remoted scopes use.
func openIdentityAuthoritySession(authority vfs.Authority) (handle.ID, string) {
	var pair [2]uint64
	if !createChannel(&pair) {
		return 0, "IDENTITY_AUTHORITY_CHANNEL_FAILED"
	}
	correlation++
	request, ok := vfs.AddSessionWithAuthorityRequest(handle.ID(pair[1]), authority, correlation)
	if !ok {
		closePair(pair)
		return 0, "IDENTITY_AUTHORITY_SEND_FAILED"
	}
	result, status, sent := filesystemRequests.Control(handle.ID(vfsControlHandle), &request, vfsSessionDeadline)
	if !sent {
		closePair(pair)
		return 0, "IDENTITY_AUTHORITY_SEND_FAILED"
	}
	if status != ipc.StatusOK || result != vfs.StatusOK {
		closeOpen(handle.ID(pair[0]))
		return 0, "IDENTITY_AUTHORITY_REFUSED"
	}
	return handle.ID(pair[0]), ""
}

// openIdentityAdmin asks identityd over its control channel to accept an
// administration session and returns the client end for a command. The command
// cannot widen it: identityd serves the session from its own record of what it
// may do.
func openIdentityAdmin() handle.ID {
	if identityControl == 0 {
		return 0
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		return 0
	}
	correlation++
	var message ipc.Message
	if !identityproto.EncodeControlRequest(identityproto.OpAddAdmin, handle.ID(pair[1]), correlation, &message) ||
		!sendMessage(uint64(identityControl), message) {
		closePair(pair)
		return 0
	}
	reply, status := receive(uint64(identityControl), identityControlDeadline)
	result, valid := identityproto.DecodeControlReply(&reply, identityproto.OpAddAdmin, correlation)
	if status != ipc.StatusOK || !valid || result != identityproto.ResultOK {
		closeOpen(handle.ID(pair[0]))
		return 0
	}
	return handle.ID(pair[0])
}
