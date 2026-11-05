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
	"goos/kernel/ipc"
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
	// Replies a full client queue held back, oldest first, as for TCP.
	held      [udpHeldReplies]heldUDPReply
	heldFirst int
	heldCount int
}

const udpHeldReplies = 128

type heldUDPReply struct {
	reply       udpapi.Reply
	correlation uint64
}

// udpReplier sends replies on one session's channel, or holds them while
// its queue is full.
type udpReplier struct{ index int }

func (r udpReplier) Reply(out *udpapi.Reply, correlation uint64) bool {
	current := &udpSessions[r.index]
	if current.heldCount == 0 {
		switch sendUDPReply(current, out, correlation) {
		case kcall.StatusOK:
			return true
		case kcall.StatusPeerDied, kcall.StatusCorruptData:
			return false
		}
	}
	if current.heldCount == len(current.held) {
		return false
	}
	current.held[(current.heldFirst+current.heldCount)%len(current.held)] = heldUDPReply{reply: *out, correlation: correlation}
	current.heldCount++
	return true
}

func sendUDPReply(current *udpSession, out *udpapi.Reply, correlation uint64) kcall.Status {
	if !udpapi.EncodeReply(out, correlation, &reply) {
		return kcall.StatusCorruptData
	}
	size, ok := ipc.EncodeFrom(&reply, wire[:])
	if !ok {
		return kcall.StatusCorruptData
	}
	return sys.IPCSend(current.channel, wire[:size])
}

func flushUDPReplies(current *udpSession) bool {
	for current.heldCount != 0 {
		held := &current.held[current.heldFirst]
		switch sendUDPReply(current, &held.reply, held.correlation) {
		case kcall.StatusOK:
		case kcall.StatusPeerDied, kcall.StatusCorruptData:
			return false
		default:
			return true
		}
		current.heldFirst = (current.heldFirst + 1) % len(current.held)
		current.heldCount--
	}
	return true
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
	// The control codec admits quotas 1 to netapi.MaxSessionQuota only.
	if !current.state.StartQuota(&stack, index, int(control.Quota), sys.Window(txBase, txPool), sys.Window(rxBase, rxPool)) {
		closeUDPSession(index, "quota invalid")
		return netapi.ControlInvalidArgument
	}
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
	if !flushUDPReplies(current) {
		closeUDPSession(index, "")
		return
	}
	for budget := sessionBudget; current.channel != 0 && budget > 0; budget-- {
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
		if fault := current.state.Handle(&stack, &udpIn, correlation, servedAt, udpReplier{index}); fault != udpserve.FaultNone {
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
