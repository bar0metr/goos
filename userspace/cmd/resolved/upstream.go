//go:build goos_userspace

package main

// The resolver's way to its DNS servers (ADR 0195). PID 1 gives this service
// a UDP session and a network status session with ControlSetNetwork, the
// same sessions any program gets, whenever this service or the network
// service starts. userspace/upstream runs the queries over the UDP session;
// this file owns the handles and the pools and carries its messages.
//
// The network session carries the DHCP lease's DNS servers: one Servers
// request waits in the network service until the list changes.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/runtime/sys"
	"goos/userspace/udpapi"
	"goos/userspace/upstream"
)

type networkLink struct {
	generation uint64
	channel    handle.ID
	txHandle   handle.ID
	rxHandle   handle.ID
	txBase     uint64
	txSize     uint64
	rxBase     uint64
	rxSize     uint64
	// status is the network session; serversAsk is the correlation of the
	// Servers request waiting in it.
	status     handle.ID
	serversAsk uint64
	// servers is the DHCP server list last reported, under its generation.
	servers          [netapi.MaxServers][4]byte
	serverCount      int
	serverGeneration uint64
}

// udpSender sends the client's requests on the UDP session.
type udpSender struct{}

func (udpSender) Send(request *udpapi.Request, correlation uint64) bool {
	return udpapi.EncodeRequest(request, correlation, &reply) && sendOn(link.channel, &reply)
}

var (
	link        networkLink
	client      upstream.Client
	correlation uint64
	udpReply    udpapi.Reply
	netRequest  netapi.Request
	netReply    netapi.Reply
)

func nextCorrelation() uint64 {
	correlation++
	if correlation == 0 {
		correlation++
	}
	return correlation
}

// setNetwork installs new sessions. Queries on the old ones can no longer
// finish, so every lookup waiting on them fails with Reconfigured; the cache
// stays.
func setNetwork(generation uint64) netapi.ControlResult {
	channel, tx, rx, status := message.Handles[0], message.Handles[1], message.Handles[2], message.Handles[3]
	refuse := func(result netapi.ControlResult) netapi.ControlResult {
		for _, id := range [...]handle.ID{channel, tx, rx, status} {
			sys.HandleClose(id)
		}
		return result
	}
	const rw = capability.Read | capability.Write | capability.Wait
	if generation <= link.generation || !handleIs(channel, handle.TypeChannel, rw) || !handleIs(status, handle.TypeChannel, rw) ||
		!handleIs(tx, handle.TypeMemory, capability.Read|capability.Write) || !handleIs(rx, handle.TypeMemory, capability.Read) {
		return refuse(netapi.ControlInvalidArgument)
	}
	const txPool, rxPool = udpapi.TXSlots * udpapi.SlotSize, udpapi.RXSlots * udpapi.SlotSize
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, true)
	if txStatus != kcall.StatusOK || txSize < txPool {
		if txStatus == kcall.StatusOK {
			sys.VMUnmap(txBase, txSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, false)
	if rxStatus != kcall.StatusOK || rxSize < rxPool {
		sys.VMUnmap(txBase, txSize)
		if rxStatus == kcall.StatusOK {
			sys.VMUnmap(rxBase, rxSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	closeNetwork()
	link = networkLink{generation: generation, channel: channel, txHandle: tx, rxHandle: rx, status: status,
		txBase: txBase, txSize: txSize, rxBase: rxBase, rxSize: rxSize}
	client.Bind(sys.Window(txBase, txPool), sys.Window(rxBase, rxPool))
	resolver.Reset(clockNanos())
	applyServers()
	askServers()
	syslog.Info("network sessions bound", syslog.Uint("generation", generation))
	return netapi.ControlOK
}

// closeNetwork drops both sessions. The DHCP servers belonged to the lease
// the old network session reported, so they go too.
func closeNetwork() {
	if link.channel == 0 {
		return
	}
	for _, id := range [...]handle.ID{link.channel, link.txHandle, link.rxHandle, link.status} {
		sys.HandleClose(id)
	}
	sys.VMUnmap(link.txBase, link.txSize)
	sys.VMUnmap(link.rxBase, link.rxSize)
	generation := link.generation
	link = networkLink{generation: generation}
	client.Unbind()
	resolver.Reset(clockNanos())
	applyServers()
}

// askServers leaves a Servers request waiting in the network service.
func askServers() {
	link.serversAsk = nextCorrelation()
	netRequest = netapi.Request{Opcode: netapi.OpServers, Generation: link.serverGeneration}
	if !netapi.EncodeRequest(&netRequest, link.serversAsk, &reply) || !sendOn(link.status, &reply) {
		syslog.Error("network session unreachable")
		closeNetwork()
	}
}

func serviceStatus() {
	for link.status != 0 {
		status := receiveOn(link.status)
		if status == kcall.StatusNotFound {
			return
		}
		if status != kcall.StatusOK || !netapi.DecodeReply(&message, link.serversAsk, netapi.OpServers, &netReply) ||
			netReply.Result != netapi.ResultOK {
			closeAttached()
			if status != kcall.StatusPeerDied {
				syslog.Error("network session reply invalid")
			}
			closeNetwork()
			return
		}
		link.serverGeneration, link.serverCount, link.servers = netReply.Generation, int(netReply.ServerCount), netReply.Servers
		applyServers()
		askServers()
	}
}

func serviceUDP() {
	for link.channel != 0 {
		status := receiveOn(link.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			syslog.Warn("network service gone; waiting for new sessions")
			closeNetwork()
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			syslog.Error("UDP reply invalid")
			closeNetwork()
			return
		}
		answered, valid := udpapi.DecodeReply(&message, &udpReply)
		if !valid || !client.Answer(&resolver, &udpReply, answered, clockNanos(), udpSender{}) {
			syslog.Error("UDP session failed")
			closeNetwork()
			return
		}
	}
}

// sendQueries starts every query the resolver has ready.
func sendQueries(now uint64) {
	if !client.SendQueries(&resolver, now, udpSender{}) {
		syslog.Error("UDP session unreachable")
		closeNetwork()
	}
}

// releaseFinished closes the endpoints of transactions that stopped waiting.
func releaseFinished() {
	if !client.ReleaseFinished(&resolver, udpSender{}) {
		syslog.Error("UDP session unreachable")
		closeNetwork()
	}
}
