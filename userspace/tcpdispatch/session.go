// Package tcpdispatch owns a GTCP1 channel's correlations and shared slots.
package tcpdispatch

import (
	"goos/kernel/ipc"
	"goos/userspace/tcpapi"
)

const MaxPending = 32

type pending struct {
	correlation uint64
	request     tcpapi.Request
	reply       tcpapi.Reply
	complete    bool
}

// Session retains unrelated completions until their caller takes them.
// A deadline is connection-fatal: close the endpoint and drain its replies.
type Session struct {
	pending [MaxPending]pending
	tx      []byte
	rx      []byte
	send    func(*ipc.Message) bool
	next    uint64
	txOwned uint8
	rxOwned uint8
	release uint8
	failed  bool
}

// Init binds a new channel and pools. The caller must close the previous
// session's channel and pool objects before reusing this dispatcher.
func (session *Session) Init(tx, rx []byte, send func(*ipc.Message) bool) bool {
	if len(tx) != tcpapi.TXSlots*tcpapi.SlotSize || len(rx) != tcpapi.RXSlots*tcpapi.SlotSize || send == nil {
		return false
	}
	for index := range session.pending {
		session.pending[index] = pending{}
	}
	session.tx, session.rx, session.send = tx, rx, send
	session.txOwned, session.rxOwned, session.release, session.failed = 0, 0, 0, false
	return true
}

// Start copies writes into an exclusively owned TX slot before sending.
func (session *Session) Start(request tcpapi.Request, data []byte) (uint64, bool) {
	if session.failed || session.send == nil || session.next == ^uint64(0) || request.Opcode == tcpapi.OpRelease {
		return 0, false
	}
	index := -1
	for candidate := range session.pending {
		if session.pending[candidate].correlation == 0 {
			index = candidate
			break
		}
	}
	if index < 0 {
		return 0, false
	}
	if request.Opcode == tcpapi.OpWrite {
		if len(data) == 0 || len(data) > tcpapi.SlotSize {
			return 0, false
		}
		slot := -1
		for candidate := 0; candidate < tcpapi.TXSlots; candidate++ {
			if session.txOwned&(1<<candidate) == 0 {
				slot = candidate
				break
			}
		}
		if slot < 0 {
			return 0, false
		}
		request.Slot, request.Length = uint8(slot), uint16(len(data))
	} else if len(data) != 0 {
		return 0, false
	}
	correlation := session.next + 1
	var message ipc.Message
	if !tcpapi.EncodeRequest(&request, correlation, &message) {
		return 0, false
	}
	if request.Opcode == tcpapi.OpWrite {
		from := int(request.Slot) * tcpapi.SlotSize
		copy(session.tx[from:from+len(data)], data)
		session.txOwned |= 1 << request.Slot
	}
	session.pending[index] = pending{correlation: correlation, request: request}
	session.next = correlation
	if !session.send(&message) {
		if request.Opcode == tcpapi.OpWrite {
			session.txOwned &^= 1 << request.Slot
		}
		session.pending[index] = pending{}
		return 0, false
	}
	return correlation, true
}

// Receive validates the completion against its outstanding request.
func (session *Session) Receive(message *ipc.Message) bool {
	var reply tcpapi.Reply
	correlation, ok := tcpapi.DecodeReply(message, &reply)
	if !ok || session.failed {
		session.failed = true
		return false
	}
	var target *pending
	for index := range session.pending {
		if session.pending[index].correlation == correlation {
			target = &session.pending[index]
			break
		}
	}
	if target == nil {
		// A stale reply can name a slot leased to a newer request. Do not
		// return that slot; the caller must replace this TCP session.
		session.failed = true
		return false
	}
	request := &target.request
	endpointMatches := reply.Endpoint == request.Endpoint
	if reply.Result == tcpapi.ResultOK && (request.Opcode == tcpapi.OpOpen || request.Opcode == tcpapi.OpAccept) {
		endpointMatches = reply.Endpoint != 0
	}
	if target.complete || reply.Opcode != request.Opcode || !endpointMatches || request.Opcode == tcpapi.OpWrite && (reply.Slot != request.Slot || reply.Length > request.Length) || request.Opcode == tcpapi.OpRead && reply.Length > request.Length {
		session.failed = true
		return false
	}
	if reply.Opcode == tcpapi.OpWrite {
		session.txOwned &^= 1 << reply.Slot
	}
	if reply.Opcode == tcpapi.OpRead && reply.Result == tcpapi.ResultOK {
		bit := uint8(1) << reply.Slot
		if session.rxOwned&bit != 0 || session.release&bit != 0 {
			session.failed = true
			return false
		}
		session.rxOwned |= bit
	}
	target.reply, target.complete = reply, true
	return true
}

// Take copies a read before returning its RX lease. A small buffer keeps it queued.
func (session *Session) Take(correlation uint64, data []byte, out *tcpapi.Reply) bool {
	if correlation == 0 || session.failed {
		return false
	}
	for index := range session.pending {
		item := &session.pending[index]
		if item.correlation != correlation || !item.complete {
			continue
		}
		reply := item.reply
		if reply.Opcode == tcpapi.OpRead && reply.Result == tcpapi.ResultOK {
			if len(data) < int(reply.Length) {
				return false
			}
			from := int(reply.Slot) * tcpapi.SlotSize
			copy(data, session.rx[from:from+int(reply.Length)])
			bit := uint8(1) << reply.Slot
			session.rxOwned &^= bit
			session.release |= bit
		}
		*out = reply
		*item = pending{}
		return true
	}
	return false
}

// Flush returns leases only after netd accepts their release notifications.
func (session *Session) Flush() bool {
	if session.failed || session.send == nil {
		return false
	}
	for slot := uint8(0); slot < tcpapi.RXSlots; slot++ {
		bit := uint8(1) << slot
		if session.release&bit == 0 {
			continue
		}
		var message ipc.Message
		request := tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: slot}
		tcpapi.EncodeRequest(&request, 0, &message)
		if !session.send(&message) {
			return false
		}
		session.release &^= bit
	}
	return true
}

func (session *Session) Failed() bool { return session.failed }

func (session *Session) Pending() int {
	count := 0
	for index := range session.pending {
		if session.pending[index].correlation != 0 {
			count++
		}
	}
	return count
}
