//go:build goos_userspace

package main

// The network service and its driver (ADR 0191). PID 1 runs both from the
// installed system in normal mode, after the configuration service. It holds
// the network function's device capabilities for the life of the system and
// hands them to each driver it starts. For every driver it starts it creates
// a new channel and two new frame pools, tells the network service about them
// over the service's control channel with a new generation, and only then
// launches the driver. Commands that ask for the network get a private
// session each, which PID 1 asks the service to accept.

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	configproto "goos/userspace/config"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/netdev"
	"goos/userspace/runtime/sys"
	"goos/userspace/udpapi"
)

const (
	netServiceName = "netd"
	netDriverName  = "virtio-net"
	// netConfigScope is all the network service may see of the
	// configuration, read-only.
	netConfigScope = "system.net."
	// The network function's handles, which the kernel installs after the
	// console handles. Without a function they hold inert objects that lack
	// Transfer.
	netPortHandle = handle.ID(0x000000010000001c)
	netIRQHandle  = handle.ID(0x000000010000001d)
	netDMAHandle  = handle.ID(0x000000010000001e)
	netPortSlot   = 0x1c - 1
)

var (
	// netControl is PID 1's end of the network service's control channel.
	netControl handle.ID
	// netGeneration numbers device bindings; it only grows within a boot.
	netGeneration uint64
	// netServiceStarted and netDriverStarted mark the first launch of each;
	// later launches renew what the first consumed.
	netServiceStarted bool
	netDriverStarted  bool
	// netDevicePresent records whether the kernel found a usable function.
	netDevicePresent bool
	// The ends of the binding the next driver launch uses: the service's
	// channel end, which moves to it with the binding, and the driver's.
	netServiceEnd, netDriverChannel, netRXPool, netTXPool handle.ID
	// netBindingFresh says those were made and not yet bound.
	netBindingFresh bool
	// The process handles of both, kept so recovery can stop them and the
	// validation build can kill the driver.
	netServiceProcess, netDriverProcess handle.ID
)

// networkDevicePresent reports whether PID 1 holds a live network function:
// the kernel gives an absent one's slot an inert object without Transfer.
func networkDevicePresent() bool {
	var record [kcall.HandleInfoSize]byte
	if sys.HandleInfo(netPortSlot, &record) != kcall.StatusOK {
		return false
	}
	id := handle.ID(uint64(record[0]) | uint64(record[1])<<8 | uint64(record[2])<<16 | uint64(record[3])<<24 |
		uint64(record[4])<<32 | uint64(record[5])<<40 | uint64(record[6])<<48 | uint64(record[7])<<56)
	rights := capability.Rights(uint32(record[12]) | uint32(record[13])<<8 | uint32(record[14])<<16 | uint32(record[15])<<24)
	return id == netPortHandle && handle.Type(record[8]) == handle.TypeDevice && rights&capability.Transfer != 0
}

// startNetwork registers the network service, and the driver when there is a
// function to drive, and starts them. Nothing waits on the network: a service
// that does not start is logged and the boot goes on.
func startNetwork(ticks uint64) {
	netDevicePresent = networkDevicePresent()
	var control [2]uint64
	if !createChannel(&control) {
		syslog.Warn("network control channel unavailable")
		return
	}
	netControl = handle.ID(control[0])
	capabilities := []supervisor.Capability{
		{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
		{Name: "control", Handle: handle.ID(control[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
	}
	if settings, ok := openConfigSession(netConfigScope, configproto.Read); ok {
		capabilities = append(capabilities, supervisor.Capability{Name: "config", Handle: settings,
			Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if manager.Register(supervisor.Definition{Name: netServiceName, Executable: supervisor.ServicePath(netServiceName, boot.Mode()),
		Capabilities: capabilities, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		syslog.Warn("network service registration failed")
		closePair(control)
		netControl = 0
		return
	}
	if netDevicePresent && newNetBinding() {
		// The device capabilities stay with PID 1, as the disk's do: a
		// driver that fails is replaced by one with the same authority. The
		// channel and pools are new for each launch.
		if manager.Register(supervisor.Definition{Name: netDriverName, Executable: supervisor.ServicePath(netDriverName, boot.Mode()),
			Dependencies: []string{netServiceName},
			Capabilities: []supervisor.Capability{
				{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
				{Name: "ports", Handle: netPortHandle, Type: handle.TypeDevice, Rights: capability.Read | capability.Write},
				{Name: "irq", Handle: netIRQHandle, Type: handle.TypeIRQ, Rights: capability.Wait},
				{Name: "dma", Handle: netDMAHandle, Type: handle.TypeDevice, Rights: capability.Write},
				{Name: "service", Handle: netDriverChannel, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				{Name: "rx", Handle: netRXPool, Type: handle.TypeMemory, Rights: capability.Read | capability.Write, Consume: true},
				{Name: "tx", Handle: netTXPool, Type: handle.TypeMemory, Rights: capability.Read, Consume: true},
			}, Restart: supervisor.RestartOnFailure, FollowsDependencies: true}) != supervisor.StatusOK {
			syslog.Warn("network driver registration failed")
		}
	} else if !netDevicePresent {
		// The service still runs: it owns lo, which needs no device.
		syslog.Info("no network device")
	}
	registerResolver()
	manager.StartReady(ticks+supervisor.RestartBackoff, launchService)
}

// renewNetServiceGrants gives a restarting network service a new control
// channel and a new configuration session.
func renewNetServiceGrants(spec *supervisor.LaunchSpec) bool {
	var control [2]uint64
	if !createChannel(&control) || !renewGrant(spec, netServiceName, "control", handle.ID(control[1])) {
		closePair(control)
		return false
	}
	if netControl != 0 {
		sys.HandleClose(netControl)
	}
	netControl = handle.ID(control[0])
	renewOptionalConfig(spec, netServiceName, netConfigScope, configproto.Read)
	return true
}

// newNetBinding makes the channel and the two pools of one binding. The
// pools are created with every right; each side later receives a copy
// narrowed to what it needs: the driver writes received frames and reads
// transmitted ones, the service the reverse.
func newNetBinding() bool {
	var pair [2]uint64
	if !createChannel(&pair) {
		return false
	}
	rx, rxStatus := sys.MemoryCreate(netdev.PoolPages)
	tx, txStatus := sys.MemoryCreate(netdev.PoolPages)
	if rxStatus != kcall.StatusOK || txStatus != kcall.StatusOK {
		closePair(pair)
		for _, id := range [...]handle.ID{rx, tx} {
			if id != 0 {
				sys.HandleClose(id)
			}
		}
		syslog.Error("network pools unavailable")
		return false
	}
	netServiceEnd, netDriverChannel, netRXPool, netTXPool = handle.ID(pair[0]), handle.ID(pair[1]), rx, tx
	netBindingFresh = true
	return true
}

// bindNetDevice prepares a driver launch with a new generation and, after the
// first, a new channel and pools. The service is given its ends first, so it
// is waiting for the driver's Ready when the driver starts. PID 1 keeps
// nothing of the binding once the driver has its copies.
func bindNetDevice(spec *supervisor.LaunchSpec) bool {
	if netGeneration+1 == 0 {
		syslog.Error("network generations exhausted")
		return false
	}
	if !netBindingFresh && !newNetBinding() {
		return false
	}
	netBindingFresh = false
	serviceRX, rxStatus := sys.HandleDuplicate(netRXPool, capability.Read|capability.Transfer)
	serviceTX, txStatus := sys.HandleDuplicate(netTXPool, capability.Read|capability.Write|capability.Transfer)
	if rxStatus != kcall.StatusOK || txStatus != kcall.StatusOK {
		for _, id := range [...]handle.ID{serviceRX, serviceTX, netServiceEnd} {
			if id != 0 {
				sys.HandleClose(id)
			}
		}
		return false
	}
	netGeneration++
	if !setNetDevice(netServiceEnd, serviceRX, serviceTX, netGeneration) {
		return false
	}
	return renewGrant(spec, netDriverName, "service", netDriverChannel) && renewGrant(spec, netDriverName, "rx", netRXPool) &&
		renewGrant(spec, netDriverName, "tx", netTXPool)
}

// setNetDevice sends the service a binding; the three handles move with it.
func setNetDevice(driver, rx, tx handle.ID, generation uint64) bool {
	if netControl == 0 {
		sys.HandleClose(driver)
		sys.HandleClose(rx)
		sys.HandleClose(tx)
		return false
	}
	correlation++
	var message ipc.Message
	attachments := [...]handle.ID{driver, rx, tx}
	request := netapi.ControlRequest{Opcode: netapi.ControlSetDevice, Generation: generation}
	if !netapi.EncodeControlRequest(&request, attachments[:], correlation, &message) || !sendMessage(uint64(netControl), message) {
		sys.HandleClose(driver)
		sys.HandleClose(rx)
		sys.HandleClose(tx)
		syslog.Error("network service did not take the device binding")
		return false
	}
	return awaitNetControl(netapi.ControlSetDevice)
}

// awaitNetControl waits, bounded, for the service's answer to the request just
// sent with the current correlation.
func awaitNetControl(opcode netapi.ControlOpcode) bool {
	reply, status := receive(uint64(netControl), vfsSessionDeadline)
	var decoded netapi.ControlReply
	if status != ipc.StatusOK || !netapi.DecodeControlReply(&reply, correlation, opcode, &decoded) || decoded.Result != netapi.ControlOK {
		syslog.Warn("network service refused a control request", syslog.Uint("operation", uint64(opcode)), syslog.Uint("result", uint64(decoded.Result)))
		return false
	}
	return true
}

// openNetSession asks the network service to accept a session with the
// rights of class and returns the client end for a command. The command
// cannot widen it: the service records the class against its end.
func openNetSession(class netapi.RightsClass) (handle.ID, bool) {
	if netControl == 0 {
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
	if !netapi.EncodeControlRequest(&request, attachments[:], correlation, &message) || !sendMessage(uint64(netControl), message) {
		closePair(pair)
		return 0, false
	}
	if !awaitNetControl(netapi.ControlAddSession) {
		sys.HandleClose(handle.ID(pair[0]))
		return 0, false
	}
	return handle.ID(pair[0]), true
}

// openUDPSession asks the network service to serve a new UDP session and
// returns the client's channel, TX pool and RX pool, in that order. Each
// side's pool handles are narrowed to what it writes: the client writes the
// TX pool, the service the RX pool.
func openUDPSession() ([3]handle.ID, bool) {
	var none [3]handle.ID
	if netControl == 0 {
		return none, false
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		return none, false
	}
	tx, txStatus := sys.MemoryCreate(udpapi.TXPoolPages)
	rx, rxStatus := sys.MemoryCreate(udpapi.RXPoolPages)
	if txStatus != kcall.StatusOK || rxStatus != kcall.StatusOK {
		closePair(pair)
		closeOpen(tx)
		closeOpen(rx)
		return none, false
	}
	serviceTX, _ := sys.HandleDuplicate(tx, capability.Read|capability.Transfer)
	serviceRX, _ := sys.HandleDuplicate(rx, capability.Read|capability.Write|capability.Transfer)
	clientTX, _ := sys.HandleDuplicate(tx, capability.Read|capability.Write|capability.Transfer)
	clientRX, _ := sys.HandleDuplicate(rx, capability.Read|capability.Transfer)
	sys.HandleClose(tx)
	sys.HandleClose(rx)
	if serviceTX == 0 || serviceRX == 0 || clientTX == 0 || clientRX == 0 {
		closePair(pair)
		for _, id := range [...]handle.ID{serviceTX, serviceRX, clientTX, clientRX} {
			closeOpen(id)
		}
		return none, false
	}
	correlation++
	var message ipc.Message
	attachments := [...]handle.ID{handle.ID(pair[1]), serviceTX, serviceRX}
	request := netapi.ControlRequest{Opcode: netapi.ControlAddUDPSession, Rights: netapi.RightsUDP}
	if !netapi.EncodeControlRequest(&request, attachments[:], correlation, &message) || !sendMessage(uint64(netControl), message) {
		closePair(pair)
		for _, id := range [...]handle.ID{serviceTX, serviceRX, clientTX, clientRX} {
			closeOpen(id)
		}
		return none, false
	}
	if !awaitNetControl(netapi.ControlAddUDPSession) {
		for _, id := range [...]handle.ID{handle.ID(pair[0]), clientTX, clientRX} {
			sys.HandleClose(id)
		}
		return none, false
	}
	return [3]handle.ID{handle.ID(pair[0]), clientTX, clientRX}, true
}

// networkLaunched runs after a network service or driver started.
func networkLaunched(name string, process handle.ID) {
	switch name {
	case netServiceName:
		netServiceProcess = keepProcess(netServiceProcess, process)
		if netServiceStarted {
			syslog.Info("network service restarted")
			mark("NETD_RESTARTED_OK\n")
		}
		netServiceStarted = true
		if resolverStarted {
			bindResolverNetwork()
		}
	case resolverName:
		resolverLaunched(process)
	case netDriverName:
		netDriverProcess = keepProcess(netDriverProcess, process)
		if netDriverStarted {
			syslog.Info("network driver restarted", syslog.Uint("generation", netGeneration))
			mark("NET_DRIVER_RESTARTED_OK\n")
		}
		netDriverStarted = true
	}
}

func appendDecimal(target []byte, value uint64) []byte {
	if value >= 10 {
		target = appendDecimal(target, value/10)
	}
	return append(target, byte('0'+value%10))
}

// networkService reports whether name is one of the network services.
func networkService(name string) bool {
	return name == netServiceName || name == netDriverName || name == resolverName
}

// networkServiceNames lists the registered network services, which run from
// the volume like the session services but are not needed to keep the
// system out of recovery.
func networkServiceNames() []string {
	names := make([]string, 0, 3)
	for _, name := range [...]string{netServiceName, netDriverName, resolverName} {
		if _, found := manager.Snapshot(name); found {
			names = append(names, name)
		}
	}
	return names
}

// stopNetwork stops the network services for recovery, which runs no
// network and carries no network command.
func stopNetwork() {
	stopResolver()
	for _, name := range [...]string{netDriverName, netServiceName} {
		manager.Hold(name)
		if manager.PlanStop(name) != supervisor.StatusOK {
			continue
		}
		process := &netDriverProcess
		if name == netServiceName {
			process = &netServiceProcess
		}
		if *process != 0 {
			sys.ProcessKill(*process)
			sys.HandleClose(*process)
			*process = 0
		}
	}
	if netControl != 0 {
		sys.HandleClose(netControl)
		netControl = 0
	}
}

// injectNetDriverFault kills the network driver, in the validation build
// only, so the test sees it replaced with a new binding while storage and
// the shell keep working.
func injectNetDriverFault() bool {
	if !validation.Enabled || netDriverProcess == 0 || sys.ProcessKill(netDriverProcess) != 0 {
		return false
	}
	sys.HandleClose(netDriverProcess)
	netDriverProcess = 0
	syslog.Warn("network driver fault injected")
	mark("NET_DRIVER_FAULT_INJECTED\n")
	return true
}
