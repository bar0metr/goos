//go:build goos_userspace

package main

// The Linux socket service (ADR 0210). PID 1 runs linuxsockd from the
// installed system in normal mode, after netd, and holds the client end of
// its GLNXCTL1 control channel. It gives the service a native TCP and a
// native UDP session at every start of either service, and a family session
// for every Linux launch while the service runs: a channel and a 17-page
// pool, one end of each granted to the launch as linux-net and
// linux-net-pool, the other sent to the service with AddFamily. The family
// ends with RemoveFamily when the launch's root process exits. After a
// restart of linuxsockd each running launch gets a fresh session through
// the kernel.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	supervisor "goos/userspace/init"
	"goos/userspace/linuxsockapi"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

const (
	linuxSockName = "linuxsockd"
	// linuxFamilyLimit matches the service's family table.
	linuxFamilyLimit = 16
	// linuxSockReplies bounds the stale replies PID 1 skips while it waits
	// for the one it asked for.
	linuxSockReplies = 4
)

// linuxFamily is one Linux launch that holds a family session: PID 1's
// handle to its root process and the token the service knows it by.
type linuxFamily struct {
	process handle.ID
	token   uint64
}

var (
	linuxSockControl handle.ID
	linuxSockStarted bool
	// linuxSockLaunching is set while linuxSockLaunched gives families
	// their sessions again.
	linuxSockLaunching bool
	linuxSockProcess   handle.ID
	// linuxSockGeneration numbers the native sessions; linuxSockSession and
	// linuxSockToken number family sessions. All three only grow.
	linuxSockGeneration uint64
	linuxSockSession    uint32
	linuxSockToken      uint64
	linuxFamilies       [linuxFamilyLimit]linuxFamily
)

// startLinuxSockets registers the service after the network service. A
// service that does not start costs Linux programs their sockets and
// nothing else.
func startLinuxSockets(ticks uint64) {
	if netControl == 0 {
		return
	}
	var control [2]uint64
	if !createChannel(&control) {
		syslog.Warn("linuxsockd control channel unavailable")
		return
	}
	linuxSockControl = handle.ID(control[0])
	if manager.Register(supervisor.Definition{Name: linuxSockName, Executable: supervisor.ServicePath(linuxSockName, boot.Mode()),
		Dependencies: []string{netServiceName},
		Capabilities: []supervisor.Capability{
			{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
			{Name: "control", Handle: handle.ID(control[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		syslog.Warn("linuxsockd registration failed")
		closePair(control)
		linuxSockControl = 0
		return
	}
	manager.StartReady(ticks+supervisor.RestartBackoff, launchService)
}

// renewLinuxSockGrants gives a restarting service a new control channel.
func renewLinuxSockGrants(spec *supervisor.LaunchSpec) bool {
	var control [2]uint64
	if !createChannel(&control) || !renewGrant(spec, linuxSockName, "control", handle.ID(control[1])) {
		closePair(control)
		return false
	}
	if linuxSockControl != 0 {
		sys.HandleClose(linuxSockControl)
	}
	linuxSockControl = handle.ID(control[0])
	return true
}

// linuxSockLaunched runs after the service started: it takes native
// sessions, and every running Linux launch gets a family session again,
// since the ones it had died with the old instance.
func linuxSockLaunched(process handle.ID) {
	linuxSockProcess = keepProcess(linuxSockProcess, process)
	if linuxSockStarted {
		syslog.Info("linuxsockd restarted")
		mark("LINUXSOCKD_RESTARTED_OK\n")
	}
	linuxSockStarted = true
	bindLinuxSockBackends()
	// The supervisor marks the service running only after this hook, and
	// the new instance takes families already.
	linuxSockLaunching = true
	for index := range linuxFamilies {
		if linuxFamilies[index].process != 0 {
			reattachLinuxFamily(&linuxFamilies[index])
		}
	}
	linuxSockLaunching = false
}

// linuxSockNetworkLaunched runs after netd started. The native sessions the
// service held died with the old netd; it gets new ones.
func linuxSockNetworkLaunched() {
	if linuxSockRunning() {
		bindLinuxSockBackends()
	}
}

// linuxSockRunning reports whether family sessions can be made now.
func linuxSockRunning() bool {
	if linuxSockControl == 0 || !linuxSockStarted {
		return false
	}
	if linuxSockLaunching {
		return true
	}
	snapshot, found := manager.Snapshot(linuxSockName)
	return found && snapshot.State == supervisor.StateRunning
}

// bindLinuxSockBackends hands the service a TCP and a UDP session, each
// under a new generation. openLinuxSocketSessions asks netd for both with
// the endpoint quota the service's socket table needs (GNETCTL 5).
func bindLinuxSockBackends() {
	tcp, udp, ok := openLinuxSocketSessions()
	if !ok {
		syslog.Error("linuxsockd native sessions unavailable")
		return
	}
	for _, opcode := range [...]linuxsockapi.ControlOpcode{linuxsockapi.ControlSetTCP, linuxsockapi.ControlSetUDP} {
		session := tcp
		if opcode == linuxsockapi.ControlSetUDP {
			session = udp
		}
		linuxSockGeneration++
		request := linuxsockapi.ControlRequest{Opcode: opcode, Generation: linuxSockGeneration}
		if _, ok := linuxSockCall(&request, session[:]); !ok {
			syslog.Warn("linuxsockd refused a native session", syslog.Uint("operation", uint64(opcode)))
		}
	}
	mark("LINUXSOCKD_NATIVE_BOUND_OK\n")
}

// linuxSockCall sends one control request and waits, bounded, for its
// reply. A reply to an earlier request that timed out is skipped. The
// attachments move with the request; one that is not sent is closed.
func linuxSockCall(request *linuxsockapi.ControlRequest, attachments []handle.ID) (linuxsockapi.ControlReply, bool) {
	var decoded linuxsockapi.ControlReply
	correlation++
	var message ipc.Message
	if linuxSockControl == 0 || !linuxsockapi.EncodeControlRequest(request, attachments, correlation, &message) ||
		!sendMessage(uint64(linuxSockControl), message) {
		for _, id := range attachments {
			closeOpen(id)
		}
		return decoded, false
	}
	for range linuxSockReplies {
		reply, status := receive(uint64(linuxSockControl), vfsSessionDeadline)
		if status != ipc.StatusOK {
			return decoded, false
		}
		if linuxsockapi.DecodeControlReply(&reply, correlation, request.Opcode, &decoded) {
			return decoded, decoded.Result == linuxsockapi.ControlOK
		}
	}
	return decoded, false
}

// linuxNetSession is a family session being made: the ends for the launch
// and the token the service took it under.
type linuxNetSession struct {
	channel handle.ID
	pool    handle.ID
	token   uint64
}

// openLinuxFamily makes a family session and gives the service its ends.
// It fails, and the launch goes without the roles, when the service is not
// running or, when record asks for a new record, PID 1's table is full.
func openLinuxFamily(record bool) (linuxNetSession, bool) {
	var none linuxNetSession
	if !linuxSockRunning() || linuxSockSession+1 == 0 || record && freeLinuxFamily() == nil {
		return none, false
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		return none, false
	}
	pool, status := sys.MemoryCreate(linuxsockapi.PoolPages)
	if status != kcall.StatusOK {
		closePair(pair)
		return none, false
	}
	servicePool, _ := sys.HandleDuplicate(pool, capability.Read|capability.Write|capability.Transfer)
	launchPool, _ := sys.HandleDuplicate(pool, capability.Read|capability.Write|capability.Transfer)
	sys.HandleClose(pool)
	if servicePool == 0 || launchPool == 0 {
		closePair(pair)
		closeOpen(servicePool)
		closeOpen(launchPool)
		return none, false
	}
	linuxSockSession++
	linuxSockToken++
	request := linuxsockapi.ControlRequest{Opcode: linuxsockapi.ControlAddFamily, Session: linuxSockSession, Token: linuxSockToken}
	attachments := [...]handle.ID{handle.ID(pair[1]), servicePool}
	if _, ok := linuxSockCall(&request, attachments[:]); !ok {
		sys.HandleClose(handle.ID(pair[0]))
		sys.HandleClose(launchPool)
		return none, false
	}
	return linuxNetSession{channel: handle.ID(pair[0]), pool: launchPool, token: linuxSockToken}, true
}

// closeLinuxFamily withdraws a session whose launch did not happen.
func closeLinuxFamily(session linuxNetSession) {
	closeOpen(session.channel)
	closeOpen(session.pool)
	if session.token != 0 {
		removeLinuxFamily(session.token)
	}
}

func removeLinuxFamily(token uint64) {
	if !linuxSockRunning() {
		return
	}
	request := linuxsockapi.ControlRequest{Opcode: linuxsockapi.ControlRemoveFamily, Token: token}
	linuxSockCall(&request, nil)
}

func freeLinuxFamily() *linuxFamily {
	for index := range linuxFamilies {
		if linuxFamilies[index].process == 0 {
			return &linuxFamilies[index]
		}
	}
	return nil
}

// linuxFamilyLaunched records a launch that took a family session.
func linuxFamilyLaunched(session linuxNetSession, process handle.ID) {
	if record := freeLinuxFamily(); record != nil && process != 0 {
		*record = linuxFamily{process: process, token: session.token}
		return
	}
	removeLinuxFamily(session.token)
}

// linuxLaunchExited ends the family session of a launch whose root process
// has exited. PID 1 is about to close its handle to the process.
func linuxLaunchExited(process handle.ID) {
	for index := range linuxFamilies {
		record := &linuxFamilies[index]
		if record.process == process && process != 0 {
			removeLinuxFamily(record.token)
			*record = linuxFamily{}
			return
		}
	}
}

// reattachLinuxFamily gives a running launch a fresh family session after
// the service restarted. A launch that cannot get one keeps running without
// sockets, and its record goes.
func reattachLinuxFamily(record *linuxFamily) {
	session, ok := openLinuxFamily(false)
	if !ok {
		syslog.Warn("Linux family session not reopened")
		*record = linuxFamily{}
		return
	}
	if !linuxNetAttach(record.process, session.channel, session.pool) {
		syslog.Warn("Linux family session not renewed")
		closeLinuxFamily(session)
		*record = linuxFamily{}
		return
	}
	record.token = session.token
	mark("LINUX_NET_REATTACHED_OK\n")
}

// stopLinuxSockets stops the service for recovery, which runs no network.
func stopLinuxSockets() {
	manager.Hold(linuxSockName)
	if manager.PlanStop(linuxSockName) == supervisor.StatusOK && linuxSockProcess != 0 {
		sys.ProcessKill(linuxSockProcess)
		sys.HandleClose(linuxSockProcess)
		linuxSockProcess = 0
	}
	if linuxSockControl != 0 {
		sys.HandleClose(linuxSockControl)
		linuxSockControl = 0
	}
	for index := range linuxFamilies {
		linuxFamilies[index] = linuxFamily{}
	}
}

// linuxSocketServiceNames lists the service when it is registered. It runs
// from the volume, like the network services, and is not needed to keep the
// system out of recovery.
func linuxSocketServiceNames() []string {
	if _, found := manager.Snapshot(linuxSockName); found {
		return []string{linuxSockName}
	}
	return nil
}
