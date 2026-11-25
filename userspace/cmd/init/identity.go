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
	// identityControlDeadline bounds the wait for identityd's control reply.
	identityControlDeadline = 2 * clock.TicksPerSecond
)

var (
	// identityControl is PID 1's end of identityd's control channel.
	identityControl handle.ID
	// identityProcess is kept so recovery can stop the service.
	identityProcess handle.ID
	// identityScopeInstalled records that the identity subtree scope is
	// installed in the current VFS namespace. vfsRestarted clears it so a
	// rebuilt namespace gets the scope back before ordinary sessions return.
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

// protectIdentityStorage creates /state/identity through PID 1's trusted setup
// session, verifies it is the directory identityd expects, and installs the
// subtree scope that denies Read|Write to every session without the identity
// grant. It is idempotent within one VFS lifetime and runs before any ordinary
// session can be published, so an unconfined session can still create the
// directory but a later one cannot reach it.
func protectIdentityStorage() bool {
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
	if !installIdentityScope() {
		return false
	}
	identityScopeInstalled = true
	return true
}

// installIdentityScope binds the reserved slot to the identity subtree and
// denies Read|Write to every session without the identity grant. Slots cannot
// be replaced within a namespace lifetime, so this runs once per lifetime.
func installIdentityScope() bool {
	correlation++
	request, ok := vfs.InstallScopeRequest(identityScopeSlot, []byte(identityDirectory), true, vfs.Read|vfs.Write, 0, correlation)
	if !ok {
		return false
	}
	result, status, sent := filesystemRequests.Control(handle.ID(vfsControlHandle), &request, vfsSessionDeadline)
	if !sent {
		return false
	}
	return status == ipc.StatusOK && result == vfs.StatusOK
}

// openIdentityVFS ensures the identity subtree scope is installed and opens
// identityd's VFS session with both the subtree's Read|Write grant and a
// canonical root confining it to /state/identity. Root confinement alone
// leaves ordinary sessions able to reach the database, and a grant alone
// would let identityd escape its directory, so the session has both.
func openIdentityVFS() (handle.ID, string) {
	if !protectIdentityStorage() {
		return 0, "IDENTITY_PROTECTION_FAILED"
	}
	authority, valid := identityAuthority([]byte(identityDirectory))
	if !valid {
		return 0, "IDENTITY_ROOT_INVALID"
	}
	return openIdentityAuthoritySession(authority)
}

// ensureIdentityDirectory creates /state and /state/identity when they are
// missing and verifies both are directories, refusing a file or a symlink
// where the database root must be. /state is the persistent volume's mount,
// already present in normal mode; identityd's own directory is made through
// PID 1's unconfined session.
func ensureIdentityDirectory(files handle.ID) bool {
	for _, prefix := range [...]string{"/state", identityDirectory} {
		stat, ok := callSession(files, vfs.Request{Operation: vfs.OperationStat, Path: []byte(prefix), Rights: vfs.NoFollow})
		if ok && stat.Status == vfs.StatusOK {
			if stat.Kind != ramfs.KindDirectory {
				return false
			}
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

// openIdentityLookup mirrors openIdentityAdmin but asks identityd for a
// lookup-only session over the same control channel. A lookup session can run
// only LookupAuthorizeKey and LookupGetAccount: it cannot provision, reset, or
// mutate, and no client-supplied field widens it. It is intentionally not wired
// to production SSH workers yet (M4 FIX §5); the VM gate exercises the actual
// endpoint.
func openIdentityLookup() handle.ID {
	if identityControl == 0 {
		return 0
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		return 0
	}
	correlation++
	var message ipc.Message
	if !identityproto.EncodeControlRequest(identityproto.OpAddLookup, handle.ID(pair[1]), correlation, &message) ||
		!sendMessage(uint64(identityControl), message) {
		closePair(pair)
		return 0
	}
	reply, status := receive(uint64(identityControl), identityControlDeadline)
	result, valid := identityproto.DecodeControlReply(&reply, identityproto.OpAddLookup, correlation)
	if status != ipc.StatusOK || !valid || result != identityproto.ResultOK {
		closeOpen(handle.ID(pair[0]))
		return 0
	}
	return handle.ID(pair[0])
}
