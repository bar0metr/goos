package tcpserve

import (
	"encoding/binary"
	"testing"

	"goos/userspace/network"
	"goos/userspace/tcpapi"
)

var (
	hostMAC = network.MAC{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}
	peerMAC = network.MAC{0x52, 0x54, 0x00, 0xaa, 0xbb, 0xcc}
	hostIP  = network.IPv4{10, 0, 2, 15}
	peerIP  = network.IPv4{10, 0, 2, 20}
	gateway = network.IPv4{10, 0, 2, 2}
)

const (
	flagFIN = 0x01
	flagSYN = 0x02
	flagRST = 0x04
	flagACK = 0x10
)

// segment is a TCP segment between the stack and the scripted peer.
type segment struct {
	sourcePort, destinationPort uint16
	sequence, acknowledgment    uint32
	flags                       uint8
	window                      uint16
	payload                     []byte
}

// remote drives a stack on net0 through frames. The peer answers ARP and
// sends only the TCP segments a test scripts.
type remote struct {
	t       *testing.T
	stack   *network.Stack
	now     uint64
	address network.IPv4
	buffer  [network.MaxFrame]byte
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	r := &remote{t: t, stack: new(network.Stack), now: 1_000_000_000_000, address: hostIP}
	r.stack.Init(1234)
	r.stack.Configure(network.Config{Mode: network.ModeStatic, Address: hostIP, Prefix: 24, Gateway: gateway}, r.now)
	r.stack.LinkUp(hostMAC, network.MTU, r.now)
	for step := 0; r.stack.Snapshot(r.now).State != network.StateReady; step++ {
		if step == 100 {
			t.Fatal("net0 did not become ready")
		}
		r.step()
	}
	r.stack.Receive(r.arpReply(), r.now)
	r.sent()
	return r
}

func (r *remote) step() {
	r.sent()
	deadline, ok := r.stack.Deadline()
	if !ok {
		r.t.Fatal("no deadline")
	}
	r.now = deadline
	r.stack.Advance(r.now)
}

// run steps through timers until limit and returns the TCP segments sent.
func (r *remote) run(limit uint64) []segment {
	var sent []segment
	for {
		sent = append(sent, r.sent()...)
		deadline, ok := r.stack.Deadline()
		if !ok || deadline > limit {
			return sent
		}
		r.now = deadline
		r.stack.Advance(r.now)
	}
}

// sent drains outgoing frames, answers ARP for the peer and returns TCP
// segments.
func (r *remote) sent() []segment {
	var segments []segment
	for {
		length, ok := r.stack.NextFrame(r.buffer[:])
		if !ok {
			return segments
		}
		frame := r.buffer[:length]
		switch binary.BigEndian.Uint16(frame[12:14]) {
		case 0x0806:
			if binary.BigEndian.Uint16(frame[20:22]) == 1 && network.IPv4(frame[38:42]) == peerIP {
				r.stack.Receive(r.arpReply(), r.now)
			}
		case 0x0800:
			ip := frame[14:]
			if ip[9] != 6 {
				continue
			}
			tcp := ip[int(ip[0]&0x0f)*4 : binary.BigEndian.Uint16(ip[2:4])]
			offset := int(tcp[12]>>4) * 4
			segments = append(segments, segment{
				sourcePort: binary.BigEndian.Uint16(tcp[0:2]), destinationPort: binary.BigEndian.Uint16(tcp[2:4]),
				sequence: binary.BigEndian.Uint32(tcp[4:8]), acknowledgment: binary.BigEndian.Uint32(tcp[8:12]),
				flags: tcp[13], window: binary.BigEndian.Uint16(tcp[14:16]), payload: append([]byte(nil), tcp[offset:]...)})
		}
	}
}

func (r *remote) arpReply() []byte {
	frame := make([]byte, 42)
	copy(frame[0:6], hostMAC[:])
	copy(frame[6:12], peerMAC[:])
	binary.BigEndian.PutUint16(frame[12:14], 0x0806)
	arp := frame[14:]
	binary.BigEndian.PutUint16(arp[0:2], 1)
	binary.BigEndian.PutUint16(arp[2:4], 0x0800)
	arp[4], arp[5] = 6, 4
	binary.BigEndian.PutUint16(arp[6:8], 2)
	copy(arp[8:14], peerMAC[:])
	copy(arp[14:18], peerIP[:])
	copy(arp[18:24], hostMAC[:])
	copy(arp[24:28], r.address[:])
	return frame
}

// send delivers a TCP segment from the peer.
func (r *remote) send(s segment) {
	tcp := make([]byte, 20+len(s.payload))
	binary.BigEndian.PutUint16(tcp[0:2], s.sourcePort)
	binary.BigEndian.PutUint16(tcp[2:4], s.destinationPort)
	binary.BigEndian.PutUint32(tcp[4:8], s.sequence)
	binary.BigEndian.PutUint32(tcp[8:12], s.acknowledgment)
	tcp[12], tcp[13] = 5<<4, s.flags
	binary.BigEndian.PutUint16(tcp[14:16], s.window)
	copy(tcp[20:], s.payload)
	pseudo := make([]byte, 12)
	copy(pseudo[0:4], peerIP[:])
	copy(pseudo[4:8], r.address[:])
	pseudo[9] = 6
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(tcp)))
	binary.BigEndian.PutUint16(tcp[16:18], checksum(pseudo, tcp))
	ip := make([]byte, 20, 20+len(tcp))
	ip[0], ip[8], ip[9] = 0x45, 64, 6
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+len(tcp)))
	copy(ip[12:16], peerIP[:])
	copy(ip[16:20], r.address[:])
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip))
	frame := make([]byte, 14, 14+len(ip)+len(tcp))
	copy(frame[0:6], hostMAC[:])
	copy(frame[6:12], peerMAC[:])
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	frame = append(append(frame, ip...), tcp...)
	for len(frame) < 60 {
		frame = append(frame, 0)
	}
	r.stack.Receive(frame, r.now)
}

func checksum(parts ...[]byte) uint16 {
	var sum uint32
	for _, part := range parts {
		for index := 0; index+1 < len(part); index += 2 {
			sum += uint32(binary.BigEndian.Uint16(part[index:]))
		}
		if len(part)%2 == 1 {
			sum += uint32(part[len(part)-1]) << 8
		}
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// deferredWrite connects a session to a silent peer and fills the send
// buffer until a Write is held. It returns the held Write's correlation.
func deferredWrite(t *testing.T, r *remote, session *Session, out *codec) (uint32, segment, uint64) {
	t.Helper()
	session.Start(r.stack, 0, make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), r.now)
	id := out.call(t, r.stack, session, tcpapi.Request{Opcode: tcpapi.OpOpen}, 1, r.now, out).Endpoint
	session.Handle(r.stack, &tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: id, Address: peerIP, Port: 7}, 2, r.now, out)
	syn := r.sent()
	if len(syn) != 1 || syn[0].flags != flagSYN || len(out.sent) != 0 {
		t.Fatalf("SYN %+v replies %+v", syn, out.sent)
	}
	peer := segment{sourcePort: 7, destinationPort: syn[0].sourcePort, sequence: 5001, window: 65535}
	r.send(segment{sourcePort: 7, destinationPort: syn[0].sourcePort, sequence: 5000, acknowledgment: syn[0].sequence + 1,
		flags: flagSYN | flagACK, window: 65535})
	session.Deliver(r.stack, r.now, out)
	if reply := out.take(t, 2); reply.Result != tcpapi.ResultOK {
		t.Fatalf("connect %+v", reply)
	}
	for correlation := uint64(3); correlation < 12; correlation++ {
		session.Handle(r.stack, &tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: id, Length: tcpapi.SlotSize}, correlation, r.now, out)
		if len(out.sent) == 0 {
			return id, peer, correlation
		}
		if reply := out.take(t, correlation); reply.Result != tcpapi.ResultOK {
			t.Fatalf("write %+v", reply)
		}
	}
	t.Fatal("send buffer never filled")
	return 0, segment{}, 0
}

func TestGTCP1DeferredWriteReportsReset(t *testing.T) {
	r := newRemote(t)
	var session Session
	out := new(codec)
	_, peer, held := deferredWrite(t, r, &session, out)
	peer.flags = flagRST
	r.send(peer)
	session.Deliver(r.stack, r.now, out)
	if reply := out.take(t, held); reply.Result != tcpapi.ResultReset || reply.Length != 0 {
		t.Fatalf("held write %+v", reply)
	}
}

func TestGTCP1DeferredWriteReportsTimeout(t *testing.T) {
	r := newRemote(t)
	var session Session
	out := new(codec)
	id, _, held := deferredWrite(t, r, &session, out)
	r.run(r.now + 30*60*1_000_000_000)
	session.Deliver(r.stack, r.now, out)
	if reply := out.take(t, held); reply.Result != tcpapi.ResultTimeout {
		t.Fatalf("held write %+v", reply)
	}
	if reply := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: id, Length: 1}, 100, r.now, out); reply.Result != tcpapi.ResultTimeout {
		t.Fatalf("later write %+v", reply)
	}
}

func TestGTCP1DeferredWriteReportsReconfigured(t *testing.T) {
	r := newRemote(t)
	var session Session
	out := new(codec)
	_, _, held := deferredWrite(t, r, &session, out)
	r.stack.Configure(network.Config{Mode: network.ModeStatic, Address: network.IPv4{10, 0, 2, 16}, Prefix: 24, Gateway: gateway}, r.now)
	session.Deliver(r.stack, r.now, out)
	if reply := out.take(t, held); reply.Result != tcpapi.ResultReconfigured {
		t.Fatalf("held write %+v", reply)
	}
}

func TestGTCP1ConnectSurvivesPeerFINBeforeDeliver(t *testing.T) {
	r := newRemote(t)
	var session Session
	out := new(codec)
	session.Start(r.stack, 0, make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), r.now)
	id := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpOpen}, 1, r.now, out).Endpoint
	session.Handle(r.stack, &tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: id, Address: peerIP, Port: 7}, 2, r.now, out)
	syn := r.sent()
	if len(syn) != 1 || len(out.sent) != 0 {
		t.Fatalf("SYN %+v replies %+v", syn, out.sent)
	}
	port := syn[0].sourcePort
	r.send(segment{sourcePort: 7, destinationPort: port, sequence: 5000, acknowledgment: syn[0].sequence + 1, flags: flagSYN | flagACK, window: 65535})
	r.send(segment{sourcePort: 7, destinationPort: port, sequence: 5001, acknowledgment: syn[0].sequence + 1, flags: flagFIN | flagACK, window: 65535})
	session.Deliver(r.stack, r.now, out)
	if reply := out.take(t, 2); reply.Result != tcpapi.ResultOK || reply.Address != peerIP || reply.Port != 7 {
		t.Fatalf("connect %+v", reply)
	}
	if reply := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: id, Length: 32}, 3, r.now, out); reply.Result != tcpapi.ResultEOF {
		t.Fatalf("read %+v", reply)
	}
}
