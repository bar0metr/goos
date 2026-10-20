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
// a time: the calls lock the session for memory safety, but two goroutines
// driving it at once would take each other's replies and overwrite each
// other's payload. tcpTX and tcpRX are set once by OpenTCP.
var (
	tcpSession   session[nativeIPC]
	tcpTX, tcpRX []byte
)

const tcpWait = 35 * clock.TicksPerSecond

func OpenTCP(data startup.Data) bool {
	channel, found := data.Handle("tcp")
	tx, txFound := data.Handle("tcp-tx")
	rx, rxFound := data.Handle("tcp-rx")
	if !found || !txFound || !rxFound {
		return false
	}
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, true)
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, false)
	const txPool, rxPool = tcpapi.TXSlots * tcpapi.SlotSize, tcpapi.RXSlots * tcpapi.SlotSize
	if txStatus != kcall.StatusOK || rxStatus != kcall.StatusOK || txSize < txPool || rxSize < rxPool {
		return false
	}
	tcpTX, tcpRX = sys.Window(txBase, txPool), sys.Window(rxBase, rxPool)
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
	if !tcpapi.EncodeRequest(request, correlation, &tcpSession.outgoing) || !tcpSession.send(&tcpSession.outgoing) {
		return 0, false
	}
	return correlation, true
}

func TCPWait(ticks uint64, reply *tcpapi.Reply) (uint64, bool) {
	tcpSession.hold()
	defer tcpSession.end()
	if !tcpSession.received(ticks) {
		return 0, false
	}
	return tcpapi.DecodeReply(&tcpSession.incoming, reply)
}

func TCPPayload() []byte { return tcpTX[:tcpapi.SlotSize] }

func TCPReceived(reply *tcpapi.Reply) []byte {
	offset := tcpapi.Offset(reply.Slot)
	return tcpRX[offset : offset+uint64(reply.Length)]
}

func TCPRelease(slot uint8) {
	release := tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: slot}
	tcpSession.hold()
	if tcpapi.EncodeRequest(&release, 0, &tcpSession.outgoing) {
		tcpSession.send(&tcpSession.outgoing)
	}
	tcpSession.end()
}
