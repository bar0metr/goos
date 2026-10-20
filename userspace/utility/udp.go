//go:build goos_userspace

package utility

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/udpapi"
)

// A command's UDP session (GUDP1, ADR 0195), when its manifest asked for
// GrantUDP and PID 1 granted it. The calls here are synchronous: each sends
// one request and waits for its reply, so a command has at most one request
// outstanding and uses TX slot 0 only. The session belongs to one goroutine
// at a time, as the TCP session does. udpTX and udpRX are set once by
// OpenUDP.
var (
	udpSession session[nativeIPC]
	udpTX      []byte
	udpRX      []byte
)

// udpWait bounds every reply except a RecvFrom's, which the caller bounds.
const udpWait = 2 * clock.TicksPerSecond

// OpenUDP maps the session's pools. It reports false when the command holds
// no UDP session.
func OpenUDP(data startup.Data) bool {
	channel, found := data.Handle("udp")
	tx, txFound := data.Handle("udp-tx")
	rx, rxFound := data.Handle("udp-rx")
	if !found || !txFound || !rxFound {
		return false
	}
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, true)
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, false)
	const txPool, rxPool = udpapi.TXSlots * udpapi.SlotSize, udpapi.RXSlots * udpapi.SlotSize
	if txStatus != kcall.StatusOK || rxStatus != kcall.StatusOK || txSize < txPool || rxSize < rxPool {
		return false
	}
	udpTX, udpRX = sys.Window(txBase, txPool), sys.Window(rxBase, rxPool)
	udpSession.bind(channel)
	return true
}

// UDPCall sends request and waits up to ticks, or a default bound when
// ticks is zero, for its reply. Replies to other requests that arrive first
// are dropped, and an RX slot one of them carries goes straight back. It
// returns false when the service is gone or answers out of protocol.
func UDPCall(request *udpapi.Request, ticks uint64, reply *udpapi.Reply) bool {
	sent, ok := UDPStart(request)
	if !ok {
		return false
	}
	if ticks == 0 {
		ticks = udpWait
	}
	for {
		answered, ok := UDPWait(ticks, reply)
		if !ok {
			return false
		}
		if answered == sent && reply.Opcode == request.Opcode {
			return true
		}
		if reply.Opcode == udpapi.OpRecvFrom && reply.Result == udpapi.ResultOK {
			UDPRelease(reply.Slot)
		}
	}
}

// UDPStart sends a request without waiting and returns its correlation.
func UDPStart(request *udpapi.Request) (uint64, bool) {
	if udpSession.bound() == 0 {
		return 0, false
	}
	correlation := udpSession.begin()
	defer udpSession.end()
	if !udpapi.EncodeRequest(request, correlation, &udpSession.outgoing) || !udpSession.send(&udpSession.outgoing) {
		return 0, false
	}
	return correlation, true
}

// UDPWait takes the next reply within ticks and returns its correlation.
func UDPWait(ticks uint64, reply *udpapi.Reply) (uint64, bool) {
	udpSession.hold()
	defer udpSession.end()
	if !udpSession.received(ticks) {
		return 0, false
	}
	return udpapi.DecodeReply(&udpSession.incoming, reply)
}

// UDPPayload is TX slot 0, where a command writes what it sends.
func UDPPayload() []byte { return udpTX[:udpapi.SlotSize] }

// UDPReceived is the payload of an RX slot a RecvFrom reply handed over.
func UDPReceived(reply *udpapi.Reply) []byte {
	offset := udpapi.Offset(reply.Slot)
	return udpRX[offset : offset+uint64(reply.Length)]
}

// UDPRelease hands an RX slot back.
func UDPRelease(slot uint8) {
	release := udpapi.Request{Opcode: udpapi.OpRelease, Slot: slot}
	udpSession.hold()
	if udpapi.EncodeRequest(&release, 0, &udpSession.outgoing) {
		udpSession.send(&udpSession.outgoing)
	}
	udpSession.end()
}
