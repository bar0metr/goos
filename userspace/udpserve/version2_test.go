package udpserve

import (
	"testing"

	"goos/userspace/network"
	"goos/userspace/udpapi"
)

func TestReadyBitsMatchTheStack(t *testing.T) {
	if udpapi.ReadyReadable != uint16(network.UDPReadyReadable) || udpapi.ReadyWritable != uint16(network.UDPReadyWritable) ||
		udpapi.ReadyAll != uint16(network.UDPReadyAll) {
		t.Fatal("ready bits differ from the stack")
	}
}

func TestNoWaitAndPeekRecvFrom(t *testing.T) {
	_, listener, bound, sender, from := pair(t, 7100)
	if reply := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound, Flags: udpapi.FlagNoWait}); reply.Result != udpapi.ResultWouldBlock {
		t.Fatalf("empty receive %+v", reply)
	}
	sender.sendTo(from, 7100, "datagram")
	peek := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound, Flags: udpapi.FlagNoWait | udpapi.FlagPeek})
	if peek.Result != udpapi.ResultOK || listener.payload(peek) != "datagram" || peek.Length != 8 {
		t.Fatalf("peek %+v", peek)
	}
	listener.send(udpapi.Request{Opcode: udpapi.OpRelease, Slot: peek.Slot})
	ready := listener.call(udpapi.Request{Opcode: udpapi.OpReady, Endpoint: bound})
	if ready.Mask != udpapi.ReadyReadable|udpapi.ReadyWritable || ready.Value != 8 || ready.Count != 1 {
		t.Fatalf("peek dequeued %+v", ready)
	}
	got := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound, Flags: udpapi.FlagNoWait})
	if got.Result != udpapi.ResultOK || listener.payload(got) != "datagram" {
		t.Fatalf("receive %+v", got)
	}
	if ready := listener.call(udpapi.Request{Opcode: udpapi.OpReady, Endpoint: bound}); ready.Mask != udpapi.ReadyWritable || ready.Count != 0 {
		t.Fatalf("after receive %+v", ready)
	}
}

func TestUDPWaitReadyNoLostWakeup(t *testing.T) {
	stack, listener, bound, sender, from := pair(t, 7101)
	seen := listener.call(udpapi.Request{Opcode: udpapi.OpReady, Endpoint: bound})
	// A datagram between Ready and WaitReady answers the wait at once.
	sender.sendTo(from, 7101, "a")
	wait := listener.call(udpapi.Request{Opcode: udpapi.OpWaitReady, Endpoint: bound, Mask: seen.Mask})
	if wait.Result != udpapi.ResultOK || wait.Mask != udpapi.ReadyReadable|udpapi.ReadyWritable || wait.Count != 1 {
		t.Fatalf("wait %+v", wait)
	}
	listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
	listener.send(udpapi.Request{Opcode: udpapi.OpRelease, Slot: 0})
	listener.out.take()
	held := listener.send(udpapi.Request{Opcode: udpapi.OpWaitReady, Endpoint: bound, Mask: seen.Mask})
	if replies := listener.out.take(); len(replies) != 0 {
		t.Fatalf("wait on the current mask answered %+v", replies)
	}
	if reply := listener.call(udpapi.Request{Opcode: udpapi.OpWaitReady, Endpoint: bound, Mask: seen.Mask}); reply.Result != udpapi.ResultBusy {
		t.Fatalf("second wait %+v", reply)
	}
	sender.sendTo(from, 7101, "b")
	listener.session.Deliver(stack, &listener.out)
	replies := listener.out.take()
	if len(replies) != 1 || replies[0].correlation != held || replies[0].reply.Mask&udpapi.ReadyReadable == 0 {
		t.Fatalf("held wait %+v", replies)
	}
	// Close answers a held wait first.
	held = listener.send(udpapi.Request{Opcode: udpapi.OpWaitReady, Endpoint: bound, Mask: replies[0].reply.Mask})
	closing := listener.send(udpapi.Request{Opcode: udpapi.OpClose, Endpoint: bound})
	replies = listener.out.take()
	if len(replies) != 2 || replies[0].correlation != held || replies[0].reply.Result != udpapi.ResultClosed || replies[1].correlation != closing {
		t.Fatalf("close %+v", replies)
	}
}

func TestOptionsAndMessageSize(t *testing.T) {
	_, listener, bound, sender, from := pair(t, 7102)
	if reply := sender.call(udpapi.Request{Opcode: udpapi.OpOption, Endpoint: from, Option: udpapi.OptionTTL}); reply.Value != 64 {
		t.Fatalf("TTL %+v", reply)
	}
	reply := sender.call(udpapi.Request{Opcode: udpapi.OpOption, Endpoint: from, Option: udpapi.OptionMaxPayload, Address: loopback})
	if reply.Result != udpapi.ResultOK || reply.Value != udpapi.MaxPayload {
		t.Fatalf("loopback max payload %+v", reply)
	}
	// The largest datagram arrives whole, and the reply names its length.
	length := copy(sender.tx, make([]byte, udpapi.MaxPayload))
	if reply := sender.call(udpapi.Request{Opcode: udpapi.OpSendTo, Endpoint: from, Address: loopback, Port: 7102, Length: uint16(length)}); reply.Result != udpapi.ResultOK {
		t.Fatalf("largest send %+v", reply)
	}
	if got := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound}); got.Length != udpapi.MaxPayload {
		t.Fatalf("receive %+v", got)
	}
	if reply := sender.call(udpapi.Request{Opcode: udpapi.OpOption, Endpoint: from + 1, Option: udpapi.OptionTTL}); reply.Result != udpapi.ResultInvalidArgument {
		t.Fatalf("stale endpoint %+v", reply)
	}
}

func TestMessageSizeOnTheWire(t *testing.T) {
	stack := newStack()
	now := uint64(1_000_000_000)
	stack.Configure(network.Config{Mode: network.ModeStatic, Address: network.IPv4{10, 0, 2, 15}, Prefix: 24, Gateway: network.IPv4{10, 0, 2, 2}}, now)
	stack.LinkUp(network.MAC{2, 0, 0, 0, 0, 1}, network.MTU, now)
	for step := 0; stack.Snapshot(now).State != network.StateReady && step < 100; step++ {
		deadline, _ := stack.Deadline()
		now = deadline
		stack.Advance(now)
		for {
			var frame [network.MaxFrame]byte
			if _, ok := stack.NextFrame(frame[:]); !ok {
				break
			}
		}
	}
	c := newClient(t, stack, 0)
	c.now = now
	endpoint := c.open()
	peer := [4]byte{10, 0, 2, 20}
	if reply := c.call(udpapi.Request{Opcode: udpapi.OpOption, Endpoint: endpoint, Option: udpapi.OptionMaxPayload, Address: peer}); reply.Value != network.MTU-28 {
		t.Fatalf("wire max payload %+v", reply)
	}
	length := uint16(network.MTU - 27)
	if reply := c.call(udpapi.Request{Opcode: udpapi.OpSendTo, Endpoint: endpoint, Address: peer, Port: 9, Length: length}); reply.Result != udpapi.ResultMessageSize {
		t.Fatalf("oversized send %+v", reply)
	}
}

func TestUDPQuotaSession(t *testing.T) {
	stack := newStack()
	big := &client{t: t, stack: stack, out: recorder{t: t}, now: 1, tx: make([]byte, udpapi.TXSlots*udpapi.SlotSize), rx: make([]byte, udpapi.RXSlots*udpapi.SlotSize)}
	if big.session.StartQuota(stack, 0, network.MaxUDPSessionQuota+1, big.tx, big.rx) {
		t.Fatal("quota past the maximum")
	}
	big.session.StartQuota(stack, 0, network.MaxUDPSessionQuota, big.tx, big.rx)
	for range network.MaxUDPSessionQuota {
		big.open()
	}
	if reply := big.call(udpapi.Request{Opcode: udpapi.OpOpen}); reply.Result != udpapi.ResultBusy {
		t.Fatalf("past the quota %+v", reply)
	}
	small := newClient(t, stack, 1)
	for range network.MaxUDPSessionEndpoints {
		small.open()
	}
	if reply := small.call(udpapi.Request{Opcode: udpapi.OpOpen}); reply.Result != udpapi.ResultBusy {
		t.Fatalf("past the ordinary quota %+v", reply)
	}
}
