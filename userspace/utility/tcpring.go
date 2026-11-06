//go:build goos_userspace

package utility

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpring"
)

// ringTransport carries a GTCP 4 session's channel messages for package
// tcpring. Its client belongs to one goroutine, so it needs no lock.
type ringTransport struct {
	channel handle.ID
	wire    [ipc.WireMaxSize]byte
}

// Send sends a request, or a doorbell. A full queue holds netd's attention
// already: netd takes the queued messages, and taking one drains the rings,
// so a doorbell that finds the queue full is dropped. A request waits for
// room.
func (t *ringTransport) Send(message *ipc.Message) bool {
	size, ok := ipc.EncodeFrom(message, t.wire[:])
	if !ok {
		return false
	}
	for {
		switch sys.IPCSend(t.channel, t.wire[:size]) {
		case kcall.StatusOK:
			return true
		case kcall.StatusResourceExhausted:
			if message.Kind == ipc.KindNotification {
				return true
			}
			sys.ThreadYield()
		default:
			return false
		}
	}
}

// Receive takes netd's next message. A wait that lasts tcpWait means netd
// stopped serving the session.
func (t *ringTransport) Receive(wait bool, message *ipc.Message) (bool, bool) {
	var status kcall.Status
	if wait {
		status = sys.IPCWaitReceive(t.channel, t.wire[:], tcpWait)
	} else {
		status = sys.IPCReceive(t.channel, t.wire[:])
	}
	switch status {
	case kcall.StatusOK:
	case kcall.StatusNotFound:
		return false, !wait
	default:
		return false, false
	}
	payload := int(t.wire[24]) | int(t.wire[25])<<8
	if payload > ipc.MaxPayload || !ipc.DecodeInto(t.wire[:ipc.WireHeaderSize+payload+int(t.wire[26])*8], message) || message.HandleCount != 0 {
		return false, false
	}
	return true, true
}

func (t *ringTransport) Yield() { sys.ThreadYield() }

var ringSession ringTransport

// OpenTCPRing maps the GTCP 4 session a GrantTCPRing command receives and
// returns its client. The command opens it once.
func OpenTCPRing(data startup.Data) (*tcpring.Client, bool) {
	channel, found := data.Handle("tcp")
	tx, txFound := data.Handle("tcp-tx")
	rx, rxFound := data.Handle("tcp-rx")
	if !found || !txFound || !rxFound || ringSession.channel != 0 {
		return nil, false
	}
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, true)
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, false)
	txPool, rxPool := tcpapi.Layout4.TXPool(), tcpapi.Layout4.RXPool()
	if txStatus != kcall.StatusOK || rxStatus != kcall.StatusOK || txSize < txPool || rxSize < rxPool {
		return nil, false
	}
	ringSession.channel = channel
	return tcpring.New(&ringSession, sys.Window(txBase, txPool), sys.Window(rxBase, rxPool))
}
