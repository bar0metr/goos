//go:build goos_userspace

package utility

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
)

// The command's TCP session. Like the UDP session it writes through TX slot
// 0 and takes whatever reply arrives next, so it belongs to one goroutine at
// a time. The lock protects codec storage, not the TX/RX slices exposed to
// callers: concurrent use can race payload memory and take another caller's
// reply. A failed wait disables the endpoint but does not revoke old slices.
// tcpTX and tcpRX are set once by OpenTCP or OpenTCPBulk, before any use,
// and tcpLayout with them: GTCP 2, or GTCP 3 for a GrantTCPBulk session.
var (
	tcpSession   session[nativeIPC]
	tcpTX, tcpRX []byte
	tcpLayout    = tcpapi.Layout2
)

const tcpWait = 35 * clock.TicksPerSecond

func OpenTCP(data startup.Data) bool { return openTCP(data, tcpapi.Layout2) }

// OpenTCPBulk maps the GTCP 3 session a GrantTCPBulk command receives.
func OpenTCPBulk(data startup.Data) bool { return openTCP(data, tcpapi.Layout3) }

func openTCP(data startup.Data, layout tcpapi.Layout) bool {
	channel, found := data.Handle("tcp")
	tx, txFound := data.Handle("tcp-tx")
	rx, rxFound := data.Handle("tcp-rx")
	if !found || !txFound || !rxFound {
		return false
	}
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, true)
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, false)
	txPool, rxPool := layout.TXPool(), layout.RXPool()
	if txStatus != kcall.StatusOK || rxStatus != kcall.StatusOK || txSize < txPool || rxSize < rxPool {
		return false
	}
	tcpTX, tcpRX, tcpLayout = sys.Window(txBase, txPool), sys.Window(rxBase, rxPool), layout
	tcpSession.bind(channel)
	return true
}

func TCPCall(request *tcpapi.Request, ticks uint64, reply *tcpapi.Reply) bool {
	sent, ok := TCPStart(request)
	if !ok {
		return false
	}
	if ticks == 0 {
		ticks = tcpWait
	}
	for {
		answered, ok := TCPWait(ticks, reply)
		if !ok {
			return false
		}
		if answered == sent && reply.Opcode == request.Opcode {
			return true
		}
		if reply.Opcode == tcpapi.OpRead && reply.Result == tcpapi.ResultOK {
			TCPRelease(reply.Slot)
		}
	}
}

func TCPStart(request *tcpapi.Request) (uint64, bool) {
	if tcpSession.bound() == 0 {
		return 0, false
	}
	correlation := tcpSession.begin()
	defer tcpSession.end()
	if !tcpLayout.EncodeRequest(request, correlation, &tcpSession.outgoing) || !tcpSession.send(&tcpSession.outgoing) {
		return 0, false
	}
	return correlation, true
}

func TCPWait(ticks uint64, reply *tcpapi.Reply) (uint64, bool) {
	tcpSession.hold()
	defer tcpSession.end()
	return receiveProtocol(&tcpSession, ticks, reply, tcpLayout.DecodeReply)
}

func TCPPayload() []byte { return tcpTX[:tcpLayout.SlotSize] }

// TCPSlot is TX slot index of the session, for a client that keeps several
// writes in flight.
func TCPSlot(index uint8) []byte {
	offset := tcpLayout.Offset(index)
	return tcpTX[offset : offset+uint64(tcpLayout.SlotSize)]
}

func TCPReceived(reply *tcpapi.Reply) []byte {
	offset := tcpLayout.Offset(reply.Slot)
	return tcpRX[offset : offset+uint64(reply.Length)]
}

func TCPRelease(slot uint8) {
	release := tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: slot}
	tcpSession.hold()
	if tcpLayout.EncodeRequest(&release, 0, &tcpSession.outgoing) {
		tcpSession.send(&tcpSession.outgoing)
	}
	tcpSession.end()
}

// TCPSlotSize is the session's slot size.
func TCPSlotSize() uint32 { return tcpLayout.SlotSize }
