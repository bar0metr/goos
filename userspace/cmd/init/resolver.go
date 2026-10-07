//go:build goos_userspace

package main

// The resolver service (ADR 0194). PID 1 runs it from the installed system in
// normal mode, next to the network service, with a read-only view of
// system.dns.* and of the hosts table. It reaches DNS servers only through the
// resolver transport: one channel and two datagram pools that PID 1 makes and
// hands to both services, each copy narrowed to what its holder writes. A new
// transport is made whenever either service starts, so a restart of one never
// leaves the other holding a dead binding. Commands that resolve names get a
// private session each, as they do with the network service.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	configproto "goos/userspace/config"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/runtime/sys"
)

const (
	resolverName        = "resolved"
	resolverConfigScope = "system.dns."
)

var (
	// resolverControl is PID 1's end of the resolver's control channel.
	resolverControl handle.ID
	resolverStarted bool
	resolverProcess handle.ID
	// transportGeneration numbers transport bindings; it only grows.
	transportGeneration uint64
)

// registerResolver registers the resolver after the network service. A
// resolver that cannot start costs name resolution and nothing else.
func registerResolver() {
	var control [2]uint64
	if !createChannel(&control) {
		syslog.Warn("resolver control channel unavailable")
		return
	}
	resolverControl = handle.ID(control[0])
	capabilities := []supervisor.Capability{
		{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
		{Name: "control", Handle: handle.ID(control[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
	}
	if settings, ok := openConfigSession(resolverConfigScope, configproto.Read|configproto.HostsRead); ok {
		capabilities = append(capabilities, supervisor.Capability{Name: "config", Handle: settings,
			Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if manager.Register(supervisor.Definition{Name: resolverName, Executable: supervisor.ServicePath(resolverName, boot.Mode()),
		Capabilities: capabilities, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		syslog.Warn("resolver registration failed")
		closePair(control)
		resolverControl = 0
	}
}

// renewResolverGrants gives a restarting resolver a new control channel and
// a new configuration session.
func renewResolverGrants(spec *supervisor.LaunchSpec) bool {
	var control [2]uint64
	if !createChannel(&control) || !renewGrant(spec, resolverName, "control", handle.ID(control[1])) {
		closePair(control)
		return false
	}
	if resolverControl != 0 {
		sys.HandleClose(resolverControl)
	}
	resolverControl = handle.ID(control[0])
	renewOptionalConfig(spec, resolverName, resolverConfigScope, configproto.Read|configproto.HostsRead)
	return true
}

// bindTransport makes a new transport and gives each service its ends. The
// network service gets the query pool to read and the reply pool to write;
// the resolver the reverse. PID 1 keeps nothing of it.
func bindTransport() {
	if netControl == 0 || resolverControl == 0 || transportGeneration+1 == 0 {
		return
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		syslog.Error("resolver transport channel unavailable")
		return
	}
	queries, queryStatus := sys.MemoryCreate(netapi.DatagramPoolPages)
	replies, replyStatus := sys.MemoryCreate(netapi.DatagramPoolPages)
	if queryStatus != kcall.StatusOK || replyStatus != kcall.StatusOK {
		closePair(pair)
		closeOpen(queries)
		closeOpen(replies)
		syslog.Error("resolver transport pools unavailable")
		return
	}
	netQueries, _ := sys.HandleDuplicate(queries, capability.Read|capability.Transfer)
	netReplies, _ := sys.HandleDuplicate(replies, capability.Read|capability.Write|capability.Transfer)
	resolverQueries, _ := sys.HandleDuplicate(queries, capability.Read|capability.Write|capability.Transfer)
	resolverReplies, _ := sys.HandleDuplicate(replies, capability.Read|capability.Transfer)
	sys.HandleClose(queries)
	sys.HandleClose(replies)
	if netQueries == 0 || netReplies == 0 || resolverQueries == 0 || resolverReplies == 0 {
		closePair(pair)
		for _, id := range [...]handle.ID{netQueries, netReplies, resolverQueries, resolverReplies} {
			closeOpen(id)
		}
		syslog.Error("resolver transport pools not narrowed")
		return
	}
	transportGeneration++
	toNet := [...]handle.ID{handle.ID(pair[0]), netQueries, netReplies}
	toResolver := [...]handle.ID{handle.ID(pair[1]), resolverQueries, resolverReplies}
	if !setTransport(netControl, toNet, netapi.ControlSetTransport) {
		for _, id := range toResolver {
			sys.HandleClose(id)
		}
		return
	}
	if setTransport(resolverControl, toResolver, netapi.ControlSetTransport) {
		syslog.Info("resolver transport bound", syslog.Uint("generation", transportGeneration))
	}
}

// setTransport sends one service its ends of the transport; the handles move
// with the message.
func setTransport(control handle.ID, ends [3]handle.ID, opcode netapi.ControlOpcode) bool {
	correlation++
	var message ipc.Message
	request := netapi.ControlRequest{Opcode: opcode, Generation: transportGeneration}
	if !netapi.EncodeControlRequest(&request, ends[:], correlation, &message) || !sendMessage(uint64(control), message) {
		for _, id := range ends {
			sys.HandleClose(id)
		}
		syslog.Error("service did not take the resolver transport")
		return false
	}
	reply, status := receive(uint64(control), vfsSessionDeadline)
	var decoded netapi.ControlReply
	if status != ipc.StatusOK || !netapi.DecodeControlReply(&reply, correlation, opcode, &decoded) || decoded.Result != netapi.ControlOK {
		syslog.Warn("service refused the resolver transport")
		return false
	}
	return true
}

// openResolverSession asks the resolver to accept a session of class and
// returns the client end for a command.
func openResolverSession(class netapi.RightsClass) (handle.ID, bool) {
	if resolverControl == 0 {
		return 0, false
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		return 0, false
	}
	correlation++
	var message ipc.Message
	attachments := [...]handle.ID{handle.ID(pair[1])}
	request := netapi.ControlRequest{Opcode: netapi.ControlAddSession, Rights: class}
	if !netapi.EncodeControlRequest(&request, attachments[:], correlation, &message) || !sendMessage(uint64(resolverControl), message) {
		closePair(pair)
		return 0, false
	}
	reply, status := receive(uint64(resolverControl), vfsSessionDeadline)
	var decoded netapi.ControlReply
	if status != ipc.StatusOK || !netapi.DecodeControlReply(&reply, correlation, netapi.ControlAddSession, &decoded) ||
		decoded.Result != netapi.ControlOK {
		sys.HandleClose(handle.ID(pair[0]))
		return 0, false
	}
	return handle.ID(pair[0]), true
}

// resolverLaunched runs after the resolver started.
func resolverLaunched(process handle.ID) {
	resolverProcess = keepProcess(resolverProcess, process)
	if resolverStarted {
		syslog.Info("resolver restarted")
		mark("RESOLVED_RESTARTED_OK\n")
	}
	resolverStarted = true
	if netServiceStarted {
		bindTransport()
	}
}

// stopResolver stops the resolver for recovery, which runs no network.
func stopResolver() {
	manager.Hold(resolverName)
	if manager.PlanStop(resolverName) == supervisor.StatusOK && resolverProcess != 0 {
		sys.ProcessKill(resolverProcess)
		sys.HandleClose(resolverProcess)
		resolverProcess = 0
	}
	if resolverControl != 0 {
		sys.HandleClose(resolverControl)
		resolverControl = 0
	}
}
