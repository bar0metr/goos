//go:build goos_userspace

package main

// The resolver's way to its DNS servers (ADR 0195, 0196). PID 1 gives this
// service a UDP session and a network status session with ControlSetNetwork,
// then a TCP session with ControlSetTCP under the same generation, the same
// sessions any program gets, whenever this service or the network service
// starts. userspace/upstream runs the queries over the UDP session and
// userspace/tcpupstream the fallbacks for truncated answers over the TCP
// session; this file owns the handles and the pools and carries their
// messages. A fault on the TCP session drops that session alone: the
// fallbacks on it fail and plain UDP queries go on.
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
	"goos/userspace/tcpapi"
	"goos/userspace/tcpupstream"
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

// tcpLink is the TCP session for DNS fallback, bound to the network
// generation it came with.
type tcpLink struct {
	channel  handle.ID
	txHandle handle.ID
	rxHandle handle.ID
	txBase   uint64
	txSize   uint64
	rxBase   uint64
	rxSize   uint64
}

// udpSender sends the client's requests on the UDP session.
type udpSender struct{}

func (udpSender) Send(request *udpapi.Request, correlation uint64) bool {
	return udpapi.EncodeRequest(request, correlation, &reply) && sendOn(link.channel, &reply)
}

// tcpSender sends the fallback client's requests on the TCP session.
type tcpSender struct{}

func (tcpSender) Send(request *tcpapi.Request, correlation uint64) bool {
	return tcpapi.EncodeRequest(request, correlation, &reply) && sendOn(stream.channel, &reply)
}

var (
	link        networkLink
	stream      tcpLink
	client      upstream.Client
	fallback    tcpupstream.Client
	tcpReply    tcpapi.Reply
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

// setTCP installs the TCP session that belongs to the current network
// generation. One for any other generation is refused.
func setTCP(generation uint64) netapi.ControlResult {
	channel, tx, rx := message.Handles[0], message.Handles[1], message.Handles[2]
	refuse := func(result netapi.ControlResult) netapi.ControlResult {
		for _, id := range [...]handle.ID{channel, tx, rx} {
			sys.HandleClose(id)
		}
		return result
	}
	const rw = capability.Read | capability.Write | capability.Wait
	if link.channel == 0 || generation != link.generation || !handleIs(channel, handle.TypeChannel, rw) ||
		!handleIs(tx, handle.TypeMemory, capability.Read|capability.Write) || !handleIs(rx, handle.TypeMemory, capability.Read) {
		return refuse(netapi.ControlInvalidArgument)
	}
	const txPool, rxPool = tcpapi.TXSlots * tcpapi.SlotSize, tcpapi.RXSlots * tcpapi.SlotSize
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
	closeTCP()
	stream = tcpLink{channel: channel, txHandle: tx, rxHandle: rx, txBase: txBase, txSize: txSize, rxBase: rxBase, rxSize: rxSize}
	fallback.Bind(sys.Window(txBase, txPool), sys.Window(rxBase, rxPool))
	syslog.Info("TCP session bound", syslog.Uint("generation", generation))
	return netapi.ControlOK
}

// closeTCP drops the TCP session. Fallbacks on it fail, so their lookups
// move on to the next server; UDP queries are not touched.
func closeTCP() {
	if stream.channel == 0 {
		return
	}
	for _, id := range [...]handle.ID{stream.channel, stream.txHandle, stream.rxHandle} {
		sys.HandleClose(id)
	}
	sys.VMUnmap(stream.txBase, stream.txSize)
	sys.VMUnmap(stream.rxBase, stream.rxSize)
	stream = tcpLink{}
	fallback.Unbind(&resolver, clockNanos())
}

// closeNetwork drops every session. The DHCP servers belonged to the lease
// the old network session reported, so they go too.
func closeNetwork() {
	closeTCP()
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

func serviceTCP() {
	for stream.channel != 0 {
		status := receiveOn(stream.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			syslog.Warn("TCP session gone; waiting for a new one")
			closeTCP()
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			syslog.Error("TCP reply invalid")
			closeTCP()
			return
		}
		answered, valid := tcpapi.DecodeReply(&message, &tcpReply)
		if !valid || !fallback.Answer(&resolver, &tcpReply, answered, clockNanos(), tcpSender{}) {
			syslog.Error("TCP session failed")
			closeTCP()
			return
		}
	}
}

// sendQueries starts every query and every TCP fallback the resolver has
// ready. Without a TCP session a fallback fails at once.
func sendQueries(now uint64) {
	if !client.SendQueries(&resolver, now, udpSender{}) {
		syslog.Error("UDP session unreachable")
		closeNetwork()
	}
	if !fallback.Start(&resolver, now, tcpSender{}) {
		syslog.Error("TCP session unreachable")
		closeTCP()
	}
}

// releaseFinished closes the endpoints of transactions that stopped waiting.
func releaseFinished() {
	if !client.ReleaseFinished(&resolver, udpSender{}) {
		syslog.Error("UDP session unreachable")
		closeNetwork()
	}
	if !fallback.ReleaseFinished(&resolver, tcpSender{}) {
		syslog.Error("TCP session unreachable")
		closeTCP()
	}
}
