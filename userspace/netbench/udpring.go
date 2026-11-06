//go:build goos_userspace

package netbench

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/shmring"
	"goos/userspace/startup"
	"goos/userspace/udpapi"
	"goos/userspace/utility"
)

// udpRing is one GUDP 3 session (ADR 0216) driven from one goroutine: the
// channel for open, bind, connect and close, and the rings for datagrams.
type udpRing struct {
	channel   handle.ID
	tx, rx    []byte
	rings     udpapi.Rings3
	wire      [ipc.WireMaxSize]byte
	out, in   ipc.Message
	reply     udpapi.Reply
	next      uint64
	txGen     [udpapi.TXSlots3]uint32
	rxGen     [udpapi.RXSlots3]uint32
	published bool
	// messages counts channel requests, replies and doorbells both ways.
	messages uint64
}

// udpWaitTicks bounds a wait for netd: five seconds at 250 Hz or more.
const udpWaitTicks = 5 * 250

// udpDatagram is the payload of each datagram, as netprobe's UDP bench
// sends.
const udpDatagram = 64

func (u *udpRing) send(message *ipc.Message) {
	size, ok := ipc.EncodeFrom(message, u.wire[:])
	if !ok {
		utility.Fail([]byte("udp ring"), "encode")
	}
	for {
		switch sys.IPCSend(u.channel, u.wire[:size]) {
		case kcall.StatusOK:
			u.messages++
			return
		case kcall.StatusResourceExhausted:
			if message.Kind == ipc.KindNotification {
				// netd takes the rings before each queued message.
				return
			}
			sys.ThreadYield()
		default:
			utility.Fail([]byte("udp ring"), "session gone")
		}
	}
}

// receive takes netd's next message, waiting when wait is set. It reports
// false when none was there.
func (u *udpRing) receive(wait bool) bool {
	var status kcall.Status
	if wait {
		status = sys.IPCWaitReceive(u.channel, u.wire[:], udpWaitTicks)
	} else {
		status = sys.IPCReceive(u.channel, u.wire[:])
	}
	if status == kcall.StatusNotFound && !wait {
		return false
	}
	payload := int(u.wire[24]) | int(u.wire[25])<<8
	if status != kcall.StatusOK || payload > ipc.MaxPayload || !ipc.DecodeInto(u.wire[:ipc.WireHeaderSize+payload+int(u.wire[26])*8], &u.in) {
		utility.Fail([]byte("udp ring"), "netd stopped answering")
	}
	u.messages++
	return true
}

// call sends a channel request and waits for its reply, taking the
// doorbells that come first.
func (u *udpRing) call(request udpapi.Request, what string) udpapi.Reply {
	u.next++
	if !udpapi.EncodeRequest(&request, u.next, &u.out) {
		utility.Fail([]byte(what), "encode")
	}
	u.send(&u.out)
	for {
		u.receive(true)
		if udpapi.IsDoorbell(&u.in) {
			continue
		}
		correlation, ok := udpapi.DecodeReply(&u.in, &u.reply)
		if !ok || correlation != u.next {
			utility.Fail([]byte(what), "bad reply")
		}
		if u.reply.Result != udpapi.ResultOK {
			utility.Fail([]byte(what), u.reply.Result.Text())
		}
		return u.reply
	}
}

func (u *udpRing) push(ring int, e *udpapi.Entry) {
	if u.rings.Push(ring, e) != shmring.OK {
		utility.Fail([]byte("udp ring"), "push")
	}
	u.published = true
}

// ring sends the doorbell when this side published and netd armed.
func (u *udpRing) ring() {
	if u.published && u.rings.ShouldRing() {
		udpapi.EncodeDoorbell(&u.out)
		u.send(&u.out)
	}
	u.published = false
}

// drain takes the messages netd queued without waiting: doorbells only,
// since no request is out.
func (u *udpRing) drain() {
	for u.receive(false) {
	}
}

// await waits until ring holds an entry and pops it. It sleeps on the
// doorbell only after it armed and found the ring still empty.
func (u *udpRing) await(ring int, e *udpapi.Entry) {
	for spins := 0; ; spins++ {
		switch u.rings.Pop(ring, e) {
		case shmring.OK:
			return
		case shmring.Broken:
			utility.Fail([]byte("udp ring"), "broken ring")
		}
		u.drain()
		if spins < 64 {
			sys.ThreadYield()
			continue
		}
		u.rings.Arm()
		if u.rings.Pending() {
			continue
		}
		u.receive(true)
	}
}

// RunUDPRing measures datagram request and reply over a GUDP 3 session
// between two connected loopback endpoints, both driven from this
// goroutine. Each half round sends from one endpoint, takes the datagram
// at the other, hands its slot back with fresh credit and takes the send's
// slot back.
func RunUDPRing(data startup.Data) {
	channel, tx, rx, ok := utility.OpenUDPRing(data)
	if !ok || !udpapi.ServiceReady(rx[udpapi.RXControl3:]) {
		utility.Fail(nil, "GUDP 3 session not granted")
	}
	u := &udpRing{channel: channel, tx: tx, rx: rx}
	u.rings, _ = udpapi.ClientRings3(tx[udpapi.TXControl3:], rx[udpapi.RXControl3:])
	first := u.call(udpapi.Request{Opcode: udpapi.OpOpen}, "open").Endpoint
	firstPort := u.call(udpapi.Request{Opcode: udpapi.OpBind, Endpoint: first, Address: loopback}, "bind").Port
	second := u.call(udpapi.Request{Opcode: udpapi.OpOpen}, "open").Endpoint
	secondPort := u.call(udpapi.Request{Opcode: udpapi.OpBind, Endpoint: second, Address: loopback}, "bind").Port
	u.call(udpapi.Request{Opcode: udpapi.OpConnect, Endpoint: first, Address: loopback, Port: secondPort}, "connect")
	u.call(udpapi.Request{Opcode: udpapi.OpConnect, Endpoint: second, Address: loopback, Port: firstPort}, "connect")
	u.push(udpapi.RingRelease, &udpapi.Entry{Kind: udpapi.EntryCredit, Endpoint: first, Value: 1})
	u.push(udpapi.RingRelease, &udpapi.Entry{Kind: udpapi.EntryCredit, Endpoint: second, Value: 1})
	before := u.messages
	start := utility.ClockNanos()
	for index := 0; index < rounds; index++ {
		u.exchange(first, second, uint8(index))
		u.exchange(second, first, uint8(index))
	}
	elapsed := (utility.ClockNanos() - start) / 1000
	spent := u.messages - before
	u.call(udpapi.Request{Opcode: udpapi.OpClose, Endpoint: first}, "close")
	u.call(udpapi.Request{Opcode: udpapi.OpClose, Endpoint: second}, "close")
	out := begin("native-u3-udp-request-reply")
	out = field(out, "rounds", rounds)
	out = field(out, "us-per-round", elapsed/rounds)
	finish(field(out, "msgs-per-round-x100", spent*100/rounds))
}

func (u *udpRing) exchange(from, to uint32, seed uint8) {
	const slot = 0
	u.txGen[slot]++
	payload := u.tx[udpapi.Offset(slot) : udpapi.Offset(slot)+udpDatagram]
	for index := range payload {
		payload[index] = seed + uint8(index)
	}
	u.push(udpapi.RingTX, &udpapi.Entry{Kind: udpapi.EntrySend, Slot: slot, Endpoint: from, Length: udpDatagram, Generation: u.txGen[slot]})
	u.ring()
	var e udpapi.Entry
	u.await(udpapi.RingRX, &e)
	if e.Endpoint != to || e.Length != udpDatagram || e.Generation != u.rxGen[e.Slot]+1 || u.rx[udpapi.Offset(e.Slot)] != seed {
		utility.Fail([]byte("udp ring"), "wrong datagram")
	}
	u.rxGen[e.Slot] = e.Generation
	u.push(udpapi.RingRelease, &udpapi.Entry{Kind: udpapi.EntryRelease, Slot: e.Slot, Generation: e.Generation})
	u.push(udpapi.RingRelease, &udpapi.Entry{Kind: udpapi.EntryCredit, Endpoint: to, Value: 1})
	// netd publishes the send's EntrySent before the datagram it caused,
	// so the slot is back by now; the doorbell for the Release entries
	// waits for the next send.
	var done udpapi.Entry
	u.await(udpapi.RingDone, &done)
	if done.Result != udpapi.ResultOK || done.Slot != slot {
		utility.Fail([]byte("udp send"), done.Result.Text())
	}
}
