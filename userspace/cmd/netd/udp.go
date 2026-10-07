//go:build goos_userspace

package main

// UDP sessions (GUDP1, ADR 0195). PID 1 hands this service one channel and
// two pools per session: the TX pool the client writes and this service only
// reads, and the RX pool this service writes and the client only reads. The
// session's index is the endpoints' owner in the stack, so a session reaches
// only its own endpoints. A malformed message or a slot handed back that the
// client does not hold ends the session, and every endpoint with it.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/network"
	"goos/userspace/runtime/sys"
	"goos/userspace/udpapi"
	"goos/userspace/udpserve"
)

// udpSession is one session's handles and mappings; udpserve holds the
// protocol state.
type udpSession struct {
	channel  handle.ID
	txHandle handle.ID
	rxHandle handle.ID
	txBase   uint64
	txSize   uint64
	rxBase   uint64
	rxSize   uint64
	state    udpserve.Session
}

// udpReplier sends replies on one session's channel.
type udpReplier struct{ index int }

func (r udpReplier) Reply(out *udpapi.Reply, correlation uint64) bool {
	return udpapi.EncodeReply(out, correlation, &reply) && sendOn(udpSessions[r.index].channel, &reply)
}

var (
	udpSessions [network.MaxUDPSessions]udpSession
	udpIn       udpapi.Request
)

// addUDPSession takes a session's channel and pools from PID 1.
func addUDPSession() netapi.ControlResult {
	channel, tx, rx := message.Handles[0], message.Handles[1], message.Handles[2]
	refuse := func(result netapi.ControlResult) netapi.ControlResult {
		for _, id := range [...]handle.ID{channel, tx, rx} {
			sys.HandleClose(id)
		}
		return result
	}
	if !handleIs(channel, handle.TypeChannel, capability.Read|capability.Write|capability.Wait) ||
		!handleIs(tx, handle.TypeMemory, capability.Read) || !handleIs(rx, handle.TypeMemory, capability.Read|capability.Write) {
		return refuse(netapi.ControlInvalidArgument)
	}
	index := -1
	for candidate := range udpSessions {
		if udpSessions[candidate].channel == 0 {
			index = candidate
			break
		}
	}
	if index < 0 {
		return refuse(netapi.ControlBusy)
	}
	const txPool, rxPool = udpapi.TXSlots * udpapi.SlotSize, udpapi.RXSlots * udpapi.SlotSize
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, false)
	if txStatus != kcall.StatusOK || txSize < txPool {
		if txStatus == kcall.StatusOK {
			sys.VMUnmap(txBase, txSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, true)
	if rxStatus != kcall.StatusOK || rxSize < rxPool {
		sys.VMUnmap(txBase, txSize)
		if rxStatus == kcall.StatusOK {
			sys.VMUnmap(rxBase, rxSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	current := &udpSessions[index]
	current.channel, current.txHandle, current.rxHandle = channel, tx, rx
	current.txBase, current.txSize, current.rxBase, current.rxSize = txBase, txSize, rxBase, rxSize
	current.state.Start(&stack, index, sys.Window(txBase, txPool), sys.Window(rxBase, rxPool))
	return netapi.ControlOK
}

// closeUDPSession ends a session: its endpoints close, waiting receives are
// dropped with it, and its handles and mappings go.
func closeUDPSession(index int, reason string) {
	current := &udpSessions[index]
	if current.channel == 0 {
		return
	}
	if reason != "" {
		syslog.Warn("UDP session closed", syslog.Uint("session", uint64(index)), syslog.Str("reason", reason))
	}
	current.state.End(&stack)
	for _, id := range [...]handle.ID{current.channel, current.txHandle, current.rxHandle} {
		sys.HandleClose(id)
	}
	sys.VMUnmap(current.txBase, current.txSize)
	sys.VMUnmap(current.rxBase, current.rxSize)
	current.channel, current.txHandle, current.rxHandle = 0, 0, 0
	current.txBase, current.txSize, current.rxBase, current.rxSize = 0, 0, 0, 0
}

func serviceUDP(index int) {
	current := &udpSessions[index]
	for current.channel != 0 {
		status := receiveOn(current.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			closeUDPSession(index, "")
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			closeUDPSession(index, "message invalid")
			return
		}
		correlation, valid := udpapi.DecodeRequest(&message, &udpIn)
		if !valid {
			closeAttached()
			closeUDPSession(index, "request invalid")
			return
		}
		if fault := current.state.Handle(&stack, &udpIn, correlation, clockNanos(), udpReplier{index}); fault != udpserve.FaultNone {
			closeUDPSession(index, string(fault))
			return
		}
		transmitPending()
	}
}

// deliverUDP answers every waiting RecvFrom whose endpoint has a datagram.
func deliverUDP() {
	for index := range udpSessions {
		if udpSessions[index].channel == 0 {
			continue
		}
		if fault := udpSessions[index].state.Deliver(&stack, udpReplier{index}); fault != udpserve.FaultNone {
			closeUDPSession(index, string(fault))
		}
	}
}
