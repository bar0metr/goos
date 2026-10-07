//go:build goos_userspace

package utility

import (
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
)

var (
	tcpHandle               handle.ID
	tcpTX, tcpRX            []byte
	tcpMessage, tcpIncoming ipc.Message
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
	tcpHandle, tcpTX, tcpRX = channel, sys.Window(txBase, txPool), sys.Window(rxBase, rxPool)
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
	if tcpHandle == 0 {
		return 0, false
	}
	correlation++
	if correlation == 0 {
		correlation++
	}
	if !tcpapi.EncodeRequest(request, correlation, &tcpMessage) || !tcpSend() {
		return 0, false
	}
	return correlation, true
}

func TCPWait(ticks uint64, reply *tcpapi.Reply) (uint64, bool) {
	if sys.IPCWaitReceive(tcpHandle, wire[:], ticks) != kcall.StatusOK {
		return 0, false
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &tcpIncoming) {
		return 0, false
	}
	return tcpapi.DecodeReply(&tcpIncoming, reply)
}

func tcpSend() bool {
	size, ok := ipc.EncodeFrom(&tcpMessage, wire[:])
	return ok && sys.IPCSend(tcpHandle, wire[:size]) == kcall.StatusOK
}

func TCPPayload() []byte { return tcpTX[:tcpapi.SlotSize] }

func TCPReceived(reply *tcpapi.Reply) []byte {
	offset := tcpapi.Offset(reply.Slot)
	return tcpRX[offset : offset+uint64(reply.Length)]
}

func TCPRelease(slot uint8) {
	release := tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: slot}
	if tcpapi.EncodeRequest(&release, 0, &tcpMessage) {
		tcpSend()
	}
}
