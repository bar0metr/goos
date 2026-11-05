//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/network"
	"goos/userspace/runtime/sys"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpserve"
)

type tcpSession struct {
	channel  handle.ID
	txHandle handle.ID
	rxHandle handle.ID
	txBase   uint64
	txSize   uint64
	rxBase   uint64
	rxSize   uint64
	state    tcpserve.Session
	// Replies a full client queue held back, oldest first. They go before
	// any newer one, so the client sees its replies in order.
	held      [tcpHeldReplies]heldReply
	heldFirst int
	heldCount int
}

// sessionBudget bounds the requests of one TCP or UDP session a pass of the
// event loop takes.
const sessionBudget = 8

// tcpHeldReplies bounds the replies one session may have waiting for room
// in its queue: a client with every parked request of a full quota answered
// at once, as a burst of readiness changes does.
const tcpHeldReplies = 128

type heldReply struct {
	reply       tcpapi.Reply
	correlation uint64
}

type tcpReplier struct{ index int }

// Reply sends a reply, or holds it while the client's queue is full. It
// reports false when the client is gone or holds too many.
func (r tcpReplier) Reply(out *tcpapi.Reply, correlation uint64) bool {
	current := &tcpSessions[r.index]
	if current.heldCount == 0 {
		switch sendTCPReply(current, out, correlation) {
		case kcall.StatusOK:
			return true
		case kcall.StatusPeerDied, kcall.StatusCorruptData:
			return false
		}
	}
	if current.heldCount == len(current.held) {
		return false
	}
	current.held[(current.heldFirst+current.heldCount)%len(current.held)] = heldReply{reply: *out, correlation: correlation}
	current.heldCount++
	return true
}

func sendTCPReply(current *tcpSession, out *tcpapi.Reply, correlation uint64) kcall.Status {
	if !current.state.Encode(out, correlation, &reply) {
		return kcall.StatusCorruptData
	}
	size, ok := ipc.EncodeFrom(&reply, wire[:])
	if !ok {
		return kcall.StatusCorruptData
	}
	status := sys.IPCSend(current.channel, wire[:size])
	if status == kcall.StatusOK {
		gtcpCounters(current.state.Layout().Version)[1]++
	}
	return status
}

// flushTCPReplies sends the held replies while the queue has room. It
// reports false when the client is gone.
func flushTCPReplies(current *tcpSession) bool {
	for current.heldCount != 0 {
		held := &current.held[current.heldFirst]
		switch sendTCPReply(current, &held.reply, held.correlation) {
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
	tcpSessions [network.MaxTCPSessions]tcpSession
	tcpIn       tcpapi.Request
	// gtcpMessages counts requests decoded and replies sent: GTCP 2, then 3.
	gtcpMessages [2][2]uint64
)

func gtcpCounters(version uint16) *[2]uint64 {
	if version == tcpapi.Version3 {
		return &gtcpMessages[1]
	}
	return &gtcpMessages[0]
}

func addTCPSession() netapi.ControlResult {
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
	for candidate := range tcpSessions {
		if tcpSessions[candidate].channel == 0 {
			index = candidate
			break
		}
	}
	if index < 0 {
		return refuse(netapi.ControlBusy)
	}
	layout, known := tcpapi.LayoutFor(uint16(control.TCPVersion))
	if !known {
		return refuse(netapi.ControlInvalidArgument)
	}
	txPool, rxPool := layout.TXPool(), layout.RXPool()
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
	current := &tcpSessions[index]
	current.channel, current.txHandle, current.rxHandle = channel, tx, rx
	current.txBase, current.txSize, current.rxBase, current.rxSize = txBase, txSize, rxBase, rxSize
	// The control codec admits quotas 1 to netapi.MaxSessionQuota only.
	if !current.state.StartLayout(&stack, index, int(control.Quota), layout, sys.Window(txBase, txPool), sys.Window(rxBase, rxPool), clockNanos()) {
		closeTCPSession(index, "quota invalid")
		return netapi.ControlInvalidArgument
	}
	return netapi.ControlOK
}

func closeTCPSession(index int, reason string) {
	current := &tcpSessions[index]
	if current.channel == 0 {
		return
	}
	if reason != "" {
		syslog.Warn("TCP session closed", syslog.Uint("session", uint64(index)), syslog.Str("reason", reason))
	}
	current.state.End(&stack, clockNanos())
	for _, id := range [...]handle.ID{current.channel, current.txHandle, current.rxHandle} {
		sys.HandleClose(id)
	}
	sys.VMUnmap(current.txBase, current.txSize)
	sys.VMUnmap(current.rxBase, current.rxSize)
	*current = tcpSession{}
}

func serviceTCP(index int) {
	current := &tcpSessions[index]
	if !flushTCPReplies(current) {
		closeTCPSession(index, "")
		return
	}
	// A pass takes at most sessionBudget requests; the event loop then
	// delivers what they made ready before the next batch, so a client that
	// keeps its queue full cannot hold back the answers parked requests wait
	// for.
	for budget := sessionBudget; current.channel != 0 && budget > 0; budget-- {
		status := receiveOn(current.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			closeTCPSession(index, "")
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			closeTCPSession(index, "message invalid")
			return
		}
		// A message of another GTCP version than the session's fails here.
		correlation, valid := current.state.Decode(&message, &tcpIn)
		if !valid {
			closeAttached()
			closeTCPSession(index, "request invalid")
			return
		}
		gtcpCounters(current.state.Layout().Version)[0]++
		if fault := current.state.Handle(&stack, &tcpIn, correlation, clockNanos(), tcpReplier{index}); fault != tcpserve.FaultNone {
			closeTCPSession(index, string(fault))
			return
		}
		transmitPending()
	}
}

func deliverTCP(now uint64) {
	for index := range tcpSessions {
		if tcpSessions[index].channel == 0 {
			continue
		}
		if fault := tcpSessions[index].state.Deliver(&stack, now, tcpReplier{index}); fault != tcpserve.FaultNone {
			closeTCPSession(index, string(fault))
		}
	}
}

// dumpTCP logs every TCP endpoint's transport state and every session's
// held replies and parked requests, for a stuck connection's diagnosis. It
// logs at warning level, which reaches the console, in records short enough
// for the log's field limit.
func dumpTCP() {
	stats := stack.TCPStats()
	syslog.Warn("tcp counters", syslog.Uint("rx", stats.RX), syslog.Uint("tx", stats.TX),
		syslog.Uint("local", stats.LocalSegments), syslog.Uint("split", stats.SplitSegments), syslog.Uint("rexmit", stats.Retransmits))
	syslog.Warn("tcp lookups", syslog.Uint("match", stats.MatchProbes), syslog.Uint("listener", stats.ListenerProbes),
		syslog.Uint("gtcp3-req", gtcpMessages[1][0]), syslog.Uint("gtcp3-rep", gtcpMessages[1][1]))
	for index := 0; index < stack.TCPEndpointCount(); index++ {
		e, live := stack.TCPEndpointAt(index)
		if !live {
			continue
		}
		id := syslog.Uint("id", uint64(e.ID))
		syslog.Warn("tcp endpoint", id, syslog.Uint("owner", uint64(e.Owner)), syslog.Uint("state", uint64(e.State)),
			syslog.Uint("port", uint64(e.LocalPort)), syslog.Uint("peer", uint64(e.RemotePort)), syslog.Uint("err", uint64(e.PendingError)))
		syslog.Warn("tcp send", id, syslog.Uint("len", uint64(e.SendLen)), syslog.Uint("sent", uint64(e.SendSent)),
			syslog.Uint("head", uint64(e.SendHead)), syslog.Uint("shut", boolValue(e.ShutdownWrite)), syslog.Uint("fin", boolValue(e.FINSent)))
		syslog.Warn("tcp seq", id, syslog.Uint("una", uint64(e.SndUna)), syslog.Uint("nxt", uint64(e.SndNxt)),
			syslog.Uint("rcv", uint64(e.RcvNxt)), syslog.Uint("wnd", uint64(e.PeerWindow)), syslog.Uint("cwnd", uint64(e.CWnd)))
		syslog.Warn("tcp recv", id, syslog.Uint("len", uint64(e.RecvLen)), syslog.Uint("head", uint64(e.RecvHead)))
	}
	for index := range tcpSessions {
		current := &tcpSessions[index]
		if current.channel == 0 {
			continue
		}
		syslog.Warn("tcp session", syslog.Uint("session", uint64(index)), syslog.Uint("version", uint64(current.state.Layout().Version)),
			syslog.Uint("held", uint64(current.heldCount)), syslog.Uint("parked", uint64(current.state.Waiting())),
			syslog.Uint("rx-lent", uint64(current.state.RXLentBits())))
	}
}

func boolValue(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}
