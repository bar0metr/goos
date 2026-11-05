package linuxsock

import (
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
)

// boundedChannel is netd's request queue: it holds capacity messages and
// refuses more, as the kernel's channel does.
type boundedChannel struct {
	queue    []ipc.Message
	parked   []ipc.Message
	capacity int
	gone     bool
}

func (c *boundedChannel) Send(message *ipc.Message) SendStatus {
	switch {
	case c.gone:
		return SendGone
	case len(c.queue) >= c.capacity:
		return SendFull
	}
	c.queue = append(c.queue, *message)
	return SendOK
}

// drainParked plays a netd that takes every queued request and answers
// none of them, as parked Reads and WaitReadys do. It returns them.
func (c *boundedChannel) drainParked() []ipc.Message {
	taken := c.queue
	c.parked = append(c.parked, taken...)
	c.queue = nil
	return taken
}

// take removes the newest request, queued or parked, that match accepts.
func (c *boundedChannel) take(match func(*ipc.Message) bool) (ipc.Message, bool) {
	for _, list := range []*[]ipc.Message{&c.queue, &c.parked} {
		for index := len(*list) - 1; index >= 0; index-- {
			if match(&(*list)[index]) {
				message := (*list)[index]
				*list = append((*list)[:index:index], (*list)[index+1:]...)
				return message, true
			}
		}
	}
	return ipc.Message{}, false
}

// linkBackend is a Backend over two NativeLinks and bounded channels.
type linkBackend struct {
	links    [UDP + 1]NativeLink
	channels [UDP + 1]boundedChannel
}

func (b *linkBackend) Submit(proto Proto, tag uint64, request *NativeRequest) SubmitStatus {
	return b.links[proto].Submit(&b.channels[proto], tag, request)
}

func newLinkHarness(t *testing.T, capacity int) (*harness, *linkBackend) {
	h := newHarness(t)
	b := &linkBackend{}
	b.links[TCP].Reset(TCP, h.generation[TCP], tcpapi.Layout3, make([]byte, tcpapi.Layout3.TXPool()), make([]byte, tcpapi.Layout3.RXPool()))
	b.links[UDP].Reset(UDP, h.generation[UDP], tcpapi.Layout3, make([]byte, udpapi.TXSlots*udpapi.SlotSize), make([]byte, udpapi.RXSlots*udpapi.SlotSize))
	b.channels[TCP].capacity, b.channels[UDP].capacity = capacity, capacity
	h.svc.backend = b
	return h, b
}

// fill submits WaitReadys until the channel refuses one: requests that park
// in netd and will draw no reply.
func fill(t *testing.T, b *linkBackend, proto Proto) {
	t.Helper()
	for tag := uint64(1 << 40); ; tag++ {
		// Parked requests of another endpoint.
		request := NativeRequest{Op: NativeWaitReady, Endpoint: 5}
		if proto == UDP {
			request.Endpoint = 6
		}
		if b.links[proto].Submit(&b.channels[proto], tag, &request) != SubmitAccepted {
			break
		}
	}
	if !b.links[proto].WantsWritable() {
		t.Fatal("a full queue left no writable interest")
	}
}

// requests decodes the queued GTCP 3 requests.
func tcpRequests(t *testing.T, messages []ipc.Message) []tcpapi.Request {
	t.Helper()
	var out []tcpapi.Request
	for index := range messages {
		var request tcpapi.Request
		if _, ok := tcpapi.Layout3.DecodeRequest(&messages[index], &request); !ok {
			t.Fatalf("undecodable request %+v", messages[index])
		}
		out = append(out, request)
	}
	return out
}

func TestDeferredReleaseGoesOnWritableAlone(t *testing.T) {
	link, channel := &NativeLink{}, &boundedChannel{capacity: 3}
	rx := make([]byte, tcpapi.Layout3.RXPool())
	link.Reset(TCP, 1, tcpapi.Layout3, make([]byte, tcpapi.Layout3.TXPool()), rx)
	// A read answer lends RX slot 2.
	reply := tcpapi.Reply{Opcode: tcpapi.OpRead, Endpoint: 5, Slot: 2, Length: 3}
	var message ipc.Message
	if !tcpapi.Layout3.EncodeReply(&reply, 9, &message) {
		t.Fatal("encode")
	}
	if _, completion, ok := link.Decode(&message); !ok || len(completion.Data) != 3 {
		t.Fatal("decode")
	}
	// The queue is full of requests that park: the release waits.
	for range 3 {
		channel.Send(&message)
	}
	if link.Done(channel) != SendFull || !link.WantsWritable() {
		t.Fatal("release not held back")
	}
	// netd takes the parked requests and answers none; only the writable
	// event remains.
	channel.drainParked()
	h := newHarness(t)
	if link.Writable(channel, h.svc) != SendOK || link.WantsWritable() {
		t.Fatal("writable did not send the release")
	}
	sent := tcpRequests(t, channel.queue)
	if len(sent) != 1 || sent[0].Opcode != tcpapi.OpRelease || sent[0].Slot != 2 {
		t.Fatalf("sent %+v", sent)
	}
	// The slot can be lent again.
	if _, _, ok := link.Decode(&message); !ok {
		t.Fatal("slot not reusable")
	}
}

func TestDeferredSubmissionsGoOnWritableAlone(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, h *harness, b *linkBackend, f int) (Proto, tcpapi.Opcode, NativeOp)
	}{
		{"stream write", func(t *testing.T, h *harness, b *linkBackend, f int) (Proto, tcpapi.Opcode, NativeOp) {
			id := h.connectedOver(f, b)
			fill(t, b, TCP)
			h.commit(f, 0, id, []byte("abc"), false, [4]byte{}, 0)
			return TCP, tcpapi.OpWrite, NativeWrite
		}},
		{"read ahead", func(t *testing.T, h *harness, b *linkBackend, f int) (Proto, tcpapi.Opcode, NativeOp) {
			h.connectedOver(f, b)
			b.channels[TCP].drainParked()
			fill(t, b, TCP)
			// The parked read answers; the next one finds the queue full.
			h.answerTCP(b, tcpapi.Reply{Opcode: tcpapi.OpRead, Endpoint: 77, Slot: 0, Length: 3})
			return TCP, tcpapi.OpRead, NativeRead
		}},
		{"wait ready", func(t *testing.T, h *harness, b *linkBackend, f int) (Proto, tcpapi.Opcode, NativeOp) {
			h.connectedOver(f, b)
			b.channels[TCP].drainParked()
			fill(t, b, TCP)
			// The held wait answers; holding the next one finds the queue full.
			h.answerTCP(b, tcpapi.Reply{Opcode: tcpapi.OpWaitReady, Endpoint: 77, Mask: tcpapi.ReadyConnected | tcpapi.ReadyWritable})
			return TCP, tcpapi.OpWaitReady, NativeWaitReady
		}},
		{"close", func(t *testing.T, h *harness, b *linkBackend, f int) (Proto, tcpapi.Opcode, NativeOp) {
			id := h.connectedOver(f, b)
			fill(t, b, TCP)
			h.call(f, linuxsockapi.Request{Opcode: opClose, Socket: id})
			return TCP, tcpapi.OpClose, NativeClose
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, b := newLinkHarness(t, 4)
			f := h.family(1, 7)
			proto, opcode, _ := c.run(t, h, b, f)
			// netd takes everything queued and parks it.
			b.channels[proto].drainParked()
			// No reply comes; the channel turning writable is the only event.
			if b.links[proto].Writable(&b.channels[proto], h.svc) != SendOK {
				t.Fatal("writable")
			}
			found := false
			for _, request := range tcpRequests(t, b.channels[proto].queue) {
				found = found || request.Opcode == opcode
			}
			if !found {
				t.Fatalf("%v not retried on writable", opcode)
			}
		})
	}
}

func TestDeferredDatagramGoesOnWritableAlone(t *testing.T) {
	h, b := newLinkHarness(t, 4)
	f := h.family(1, 7)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindDatagram})
	h.answerUDP(b, udpapi.Reply{Opcode: udpapi.OpOpen, Endpoint: 88})
	id := h.reply(f, correlation).Socket
	h.send(f, linuxsockapi.Request{Opcode: opBind, Socket: id, Port: 9000})
	h.answerUDP(b, udpapi.Reply{Opcode: udpapi.OpBind, Endpoint: 88, Address: [4]byte{127, 0, 0, 1}, Port: 9000})
	fill(t, b, UDP)
	h.commit(f, 0, id, []byte("query"), true, [4]byte{127, 0, 0, 1}, 53)
	b.channels[UDP].drainParked()
	b.links[UDP].Writable(&b.channels[UDP], h.svc)
	sent := false
	for index := range b.channels[UDP].queue {
		var out udpapi.Request
		if _, ok := udpapi.DecodeRequest(&b.channels[UDP].queue[index], &out); ok && out.Opcode == udpapi.OpSendTo {
			sent = true
		}
	}
	if !sent {
		t.Fatal("datagram not retried on writable")
	}
}

func TestWritableWithNothingHeldDoesNothing(t *testing.T) {
	h, b := newLinkHarness(t, 4)
	if b.links[TCP].WantsWritable() {
		t.Fatal("writable interest with nothing held")
	}
	b.channels[TCP].gone = true
	fill2 := NativeRequest{Op: NativeReady, Endpoint: 1}
	if b.links[TCP].Submit(&b.channels[TCP], 7, &fill2) != SubmitDown {
		t.Fatal("a gone peer is not down")
	}
	if b.links[TCP].Writable(&b.channels[TCP], h.svc) != SendOK {
		t.Fatal("writable without interest")
	}
}

// connectedOver opens and connects a stream whose native requests go
// through the bounded channel, answering them as netd would.
func (h *harness) connectedOver(f int, b *linkBackend) uint64 {
	h.t.Helper()
	correlation := h.send(f, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream})
	h.answerTCP(b, tcpapi.Reply{Opcode: tcpapi.OpOpen, Endpoint: 77})
	id := h.reply(f, correlation).Socket
	correlation = h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 80})
	h.answerTCP(b, tcpapi.Reply{Opcode: tcpapi.OpConnect, Endpoint: 77, Address: peerAddress, Port: 80, Local: localAddress, LocalPort: 4000})
	h.reply(f, correlation)
	return id
}

// answerTCP answers the newest queued request of the reply's opcode.
func (h *harness) answerTCP(b *linkBackend, reply tcpapi.Reply) {
	h.t.Helper()
	var correlation uint64
	message, found := b.channels[TCP].take(func(m *ipc.Message) bool {
		var request tcpapi.Request
		c, ok := tcpapi.Layout3.DecodeRequest(m, &request)
		if !ok || request.Opcode != reply.Opcode || request.Endpoint != reply.Endpoint && request.Opcode != tcpapi.OpOpen {
			return false
		}
		correlation = c
		return true
	})
	if found {
		if !tcpapi.Layout3.EncodeReply(&reply, correlation, &message) {
			h.t.Fatalf("encode %+v", reply)
		}
		tag, completion, ok := b.links[TCP].Decode(&message)
		if !ok {
			h.t.Fatal("decode")
		}
		h.svc.Complete(TCP, h.generation[TCP], tag, completion)
		b.links[TCP].Done(&b.channels[TCP])
		return
	}
	h.t.Fatalf("no queued %v", reply.Opcode)
}

// answerUDP answers the newest queued GUDP request of the reply's opcode.
func (h *harness) answerUDP(b *linkBackend, reply udpapi.Reply) {
	h.t.Helper()
	var correlation uint64
	message, found := b.channels[UDP].take(func(m *ipc.Message) bool {
		var request udpapi.Request
		c, ok := udpapi.DecodeRequest(m, &request)
		if !ok || request.Opcode != reply.Opcode {
			return false
		}
		correlation = c
		return true
	})
	if found {
		if !udpapi.EncodeReply(&reply, correlation, &message) {
			h.t.Fatalf("encode %+v", reply)
		}
		tag, completion, ok := b.links[UDP].Decode(&message)
		if !ok {
			h.t.Fatal("decode")
		}
		h.svc.Complete(UDP, h.generation[UDP], tag, completion)
		return
	}
	h.t.Fatalf("no queued %v", reply.Opcode)
}
