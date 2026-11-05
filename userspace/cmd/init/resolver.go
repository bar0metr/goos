//go:build goos_userspace

package main

// The resolver service (ADR 0194). PID 1 runs it from the installed system in
// normal mode, next to the network service, with a read-only view of
// system.dns.* and of the hosts table. It reaches DNS servers over a UDP
// session, repeats truncated answers over a TCP session, and learns the DHCP
// lease's servers over a network status session, all opened from the network
// service like any program's (ADR 0195, 0196). PID 1 opens a new set whenever
// either service starts, so a restart of one never leaves the other holding
// dead sessions. Commands that resolve names get a private session each, as
// they do with the network service.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
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
	// resolverNetworkGeneration numbers the session pairs given to the
	// resolver; it only grows.
	resolverNetworkGeneration uint64
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

// bindResolverNetwork opens a UDP session and a status session from the
// network service and hands both to the resolver, then a TCP session under
// the same generation. PID 1 keeps nothing of them. Without the TCP session
// the resolver still answers over UDP; only truncated answers fail.
func bindResolverNetwork() {
	if netControl == 0 || resolverControl == 0 || resolverNetworkGeneration+1 == 0 {
		return
	}
	udp, ok := openUDPSession()
	if !ok {
		syslog.Error("resolver UDP session unavailable")
		return
	}
	status, ok := openNetSession(netapi.RightsResolveStatus)
	if !ok {
		for _, id := range udp {
			sys.HandleClose(id)
		}
		syslog.Error("resolver network session unavailable")
		return
	}
	resolverNetworkGeneration++
	correlation++
	var message ipc.Message
	attachments := [...]handle.ID{udp[0], udp[1], udp[2], status}
	request := netapi.ControlRequest{Opcode: netapi.ControlSetNetwork, Generation: resolverNetworkGeneration}
	if !netapi.EncodeControlRequest(&request, attachments[:], correlation, &message) || !sendMessage(uint64(resolverControl), message) {
		for _, id := range attachments {
			sys.HandleClose(id)
		}
		syslog.Error("resolver did not take its network sessions")
		return
	}
	reply, replyStatus := receive(uint64(resolverControl), vfsSessionDeadline)
	var decoded netapi.ControlReply
	if replyStatus != ipc.StatusOK || !netapi.DecodeControlReply(&reply, correlation, netapi.ControlSetNetwork, &decoded) ||
		decoded.Result != netapi.ControlOK {
		syslog.Warn("resolver refused its network sessions")
		return
	}
	syslog.Info("resolver network sessions bound", syslog.Uint("generation", resolverNetworkGeneration))
	bindResolverTCP()
}

// bindResolverTCP hands the resolver a TCP session for DNS fallback under
// the network generation just bound.
func bindResolverTCP() {
	tcp, ok := openTCPSession()
	if !ok {
		syslog.Error("resolver TCP session unavailable")
		return
	}
	correlation++
	var message ipc.Message
	request := netapi.ControlRequest{Opcode: netapi.ControlSetTCP, Generation: resolverNetworkGeneration}
	if !netapi.EncodeControlRequest(&request, tcp[:], correlation, &message) || !sendMessage(uint64(resolverControl), message) {
		for _, id := range tcp {
			sys.HandleClose(id)
		}
		syslog.Error("resolver did not take its TCP session")
		return
	}
	reply, status := receive(uint64(resolverControl), vfsSessionDeadline)
	var decoded netapi.ControlReply
	if status != ipc.StatusOK || !netapi.DecodeControlReply(&reply, correlation, netapi.ControlSetTCP, &decoded) ||
		decoded.Result != netapi.ControlOK {
		syslog.Warn("resolver refused its TCP session")
		return
	}
	mark("RESOLVED_TCP_BOUND_OK\n")
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
		bindResolverNetwork()
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
