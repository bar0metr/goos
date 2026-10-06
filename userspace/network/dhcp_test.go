package network

import (
	"encoding/binary"
	"testing"
)

var (
	serverIP  = IPv4{10, 0, 2, 2}
	serverMAC = gatewayMAC
)

const testLease = 120

// clientMessage is one decoded DHCP message the stack sent.
type clientMessage struct {
	frame   sentFrame
	xid     uint32
	secs    uint16
	flags   uint16
	ciaddr  IPv4
	kind    uint8
	options map[uint8][]byte
}

func parseClient(t testing.TB, frame []byte) clientMessage {
	t.Helper()
	sent := decode(t, frame)
	if sent.ip.protocol != protocolUDP || sent.udp.sourcePort != portClient || sent.udp.destinationPort != portServer {
		t.Fatalf("not a DHCP client datagram: %+v", sent)
	}
	data := sent.udp.payload
	if len(data) < dhcpMinimumSize || data[0] != bootRequest || data[1] != hardwareEther || data[2] != hardwareLength ||
		binary.BigEndian.Uint32(data[dhcpFixedSize:dhcpOptionsAt]) != dhcpCookie {
		t.Fatalf("bad BOOTP request header")
	}
	var chaddr MAC
	copy(chaddr[:], data[dhcpChaddrAt:])
	if chaddr != hostMAC {
		t.Fatalf("chaddr %v", chaddr)
	}
	message := clientMessage{
		frame:   sent,
		xid:     binary.BigEndian.Uint32(data[4:8]),
		secs:    binary.BigEndian.Uint16(data[8:10]),
		flags:   binary.BigEndian.Uint16(data[10:12]),
		options: map[uint8][]byte{},
	}
	copy(message.ciaddr[:], data[12:16])
	options := data[dhcpOptionsAt:]
	for index := 0; index < len(options); {
		code := options[index]
		if code == optionPad {
			index++
			continue
		}
		if code == optionEnd {
			break
		}
		length := int(options[index+1])
		message.options[code] = options[index+2 : index+2+length]
		index += 2 + length
	}
	if kind := message.options[optionMessageType]; len(kind) == 1 {
		message.kind = kind[0]
	} else {
		t.Fatal("no message type")
	}
	return message
}

func onlyClient(t testing.TB, frames [][]byte) clientMessage {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want one DHCP message", len(frames))
	}
	return parseClient(t, frames[0])
}

type option struct {
	code  uint8
	value []byte
}

func u32(value uint32) []byte {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], value)
	return data[:]
}

func encodeOptions(options []option) []byte {
	var area []byte
	for _, item := range options {
		area = append(area, item.code, byte(len(item.value)))
		area = append(area, item.value...)
	}
	return append(area, optionEnd)
}

func leaseOptions(kind uint8) []option {
	return []option{
		{optionMessageType, []byte{kind}},
		{optionServerID, serverIP[:]},
		{optionSubnetMask, []byte{255, 255, 255, 0}},
		{optionRouter, serverIP[:]},
		{optionLeaseTime, u32(testLease)},
		{6, []byte{10, 0, 2, 3}},
	}
}

// bootReply builds a server reply; options is the raw options area.
type serverReply struct {
	op      byte
	htype   byte
	hlen    byte
	xid     uint32
	chaddr  MAC
	yiaddr  IPv4
	cookie  uint32
	options []byte
	sname   []byte
	file    []byte
}

func (reply serverReply) bytes() []byte {
	data := make([]byte, dhcpOptionsAt+len(reply.options))
	data[0], data[1], data[2] = reply.op, reply.htype, reply.hlen
	binary.BigEndian.PutUint32(data[4:8], reply.xid)
	copy(data[16:20], reply.yiaddr[:])
	copy(data[20:24], serverIP[:])
	copy(data[dhcpChaddrAt:], reply.chaddr[:])
	copy(data[dhcpSnameAt:dhcpSnameAt+dhcpSnameSize], reply.sname)
	copy(data[dhcpFileAt:dhcpFileAt+dhcpFileSize], reply.file)
	binary.BigEndian.PutUint32(data[dhcpFixedSize:dhcpOptionsAt], reply.cookie)
	copy(data[dhcpOptionsAt:], reply.options)
	return data
}

func replyFor(request clientMessage, yiaddr IPv4, options []option) serverReply {
	return serverReply{op: bootReply, htype: hardwareEther, hlen: hardwareLength, xid: request.xid,
		chaddr: hostMAC, yiaddr: yiaddr, cookie: dhcpCookie, options: encodeOptions(options)}
}

func dhcpFrameTo(destination IPv4, payload []byte) []byte {
	return ipv4Frame(broadcastMAC, serverMAC, serverIP, destination, protocolUDP,
		udpBytes(serverIP, destination, portServer, portClient, payload))
}

func dhcpFrame(payload []byte) []byte { return dhcpFrameTo(broadcastIPv4, payload) }

func dhcpHarness(t testing.TB) (*harness, clientMessage) {
	t.Helper()
	h := newHarness(t)
	h.stack.LinkUp(hostMAC, MTU, h.now)
	discover := onlyClient(t, h.drain())
	if discover.kind != dhcpDiscover {
		t.Fatalf("first message %d", discover.kind)
	}
	return h, discover
}

// offerAndAck runs the exchange up to the start of the conflict probe.
func offerAndAck(h *harness, discover clientMessage, ack []option) clientMessage {
	h.t.Helper()
	h.receive(dhcpFrame(replyFor(discover, hostIP, leaseOptions(dhcpOffer)).bytes()))
	request := onlyClient(h.t, h.drain())
	if request.kind != dhcpRequest || request.xid != discover.xid {
		h.t.Fatalf("request %+v", request)
	}
	h.receive(dhcpFrame(replyFor(request, hostIP, ack).bytes()))
	return request
}

func boundHarness(t testing.TB) (*harness, uint64) {
	t.Helper()
	h, discover := dhcpHarness(t)
	offerAndAck(h, discover, leaseOptions(dhcpAck))
	requested := h.now
	h.runUntil(h.now + second)
	if state := h.stack.Snapshot(h.now).State; state != StateReady {
		t.Fatalf("state %d", state)
	}
	h.drain()
	return h, requested
}

func TestDHCPHappyPath(t *testing.T) {
	h, discover := dhcpHarness(t)
	start := h.now
	if discover.frame.ether.destination != broadcastMAC || discover.frame.ip.source != zeroIPv4 ||
		discover.frame.ip.destination != broadcastIPv4 || discover.flags != dhcpBroadcast || discover.ciaddr != zeroIPv4 {
		t.Fatalf("discover %+v", discover)
	}
	if state := h.stack.Snapshot(h.now).State; state != StateConfiguring {
		t.Fatalf("state %d", state)
	}
	h.advance(10 * millisecond)
	h.receive(dhcpFrame(replyFor(discover, hostIP, leaseOptions(dhcpOffer)).bytes()))
	request := onlyClient(t, h.drain())
	if request.kind != dhcpRequest || request.xid != discover.xid || request.flags != dhcpBroadcast ||
		string(request.options[optionRequestedIP]) != string(hostIP[:]) ||
		string(request.options[optionServerID]) != string(serverIP[:]) {
		t.Fatalf("request %+v", request)
	}
	h.advance(10 * millisecond)
	h.receive(dhcpFrame(replyFor(request, hostIP, leaseOptions(dhcpAck)).bytes()))
	probe := only(t, h.drain())
	if probe.arp.senderIP != zeroIPv4 || probe.arp.targetIP != hostIP {
		t.Fatalf("probe %+v", probe.arp)
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateConfiguring || snapshot.Address != zeroIPv4 {
		t.Fatalf("address applied before the probe: %+v", snapshot)
	}
	h.advance(probeInterval)
	h.drain()
	h.advance(probeInterval)
	announce := only(t, h.drain())
	if announce.arp.senderIP != hostIP {
		t.Fatalf("announcement %+v", announce.arp)
	}
	snapshot := h.stack.Snapshot(h.now)
	if snapshot.State != StateReady || snapshot.Mode != ModeDHCP || snapshot.Address != hostIP ||
		snapshot.Prefix != 24 || snapshot.Gateway != serverIP || snapshot.LeaseRemaining != testLease-2 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	// With a responsive server the whole path takes about one second.
	if elapsed := h.now - start; elapsed > 2*second {
		t.Fatalf("DHCP took %d ns", elapsed)
	}
}

// dhcpMessages decodes the DHCP messages among frames and skips ARP.
func dhcpMessages(t testing.TB, frames [][]byte) []clientMessage {
	t.Helper()
	var messages []clientMessage
	for _, frame := range frames {
		if binary.BigEndian.Uint16(frame[12:14]) == etherTypeIPv4 {
			messages = append(messages, parseClient(t, frame))
		}
	}
	return messages
}

func TestDHCPLeaseLifecycle(t *testing.T) {
	h, requested := boundHarness(t)
	renewAt := requested + testLease*second/2
	h.runUntil(renewAt - 1)
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("renewed early")
	}
	h.runUntil(renewAt)
	renew := onlyClient(t, h.drain())
	if renew.kind != dhcpRequest || renew.ciaddr != hostIP || renew.flags != 0 ||
		renew.frame.ether.destination != serverMAC || renew.frame.ip.source != hostIP ||
		renew.frame.ip.destination != serverIP || renew.options[optionRequestedIP] != nil ||
		renew.options[optionServerID] != nil {
		t.Fatalf("renew %+v", renew)
	}

	// The renewed lease runs from when the renew request left.
	h.advance(second)
	h.receive(dhcpFrameTo(hostIP, replyFor(renew, hostIP, leaseOptions(dhcpAck)).bytes()))
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.LeaseRemaining != testLease-1 {
		t.Fatalf("after renew %+v", snapshot)
	}
	renewed := renewAt
	renewAt = renewed + testLease*second/2
	rebindAt := renewed + testLease*second/8*7
	expiry := renewed + testLease*second

	h.runUntil(renewAt)
	again := onlyClient(t, h.drain())
	if again.kind != dhcpRequest || again.frame.ip.destination != serverIP {
		t.Fatalf("second renew %+v", again)
	}
	// The retry floor of 60 s is capped at T2, so nothing more is sent before it.
	h.runUntil(rebindAt - 1)
	if frames := h.drain(); len(frames) != 0 {
		t.Fatalf("%d frames between renew and rebind", len(frames))
	}
	h.runUntil(rebindAt)
	rebind := onlyClient(t, h.drain())
	if rebind.kind != dhcpRequest || rebind.ciaddr != hostIP || rebind.frame.ip.source != hostIP ||
		rebind.frame.ip.destination != broadcastIPv4 || rebind.frame.ether.destination != broadcastMAC ||
		rebind.xid == again.xid || rebind.options[optionServerID] != nil {
		t.Fatalf("rebind %+v", rebind)
	}

	h.runUntil(expiry - second)
	if result := h.stack.Echo(0, peerIP, 1, MaxEchoTimeout, h.now); result != ResultOK {
		t.Fatalf("echo %d", result)
	}
	h.runUntil(expiry)
	completion := h.completion()
	if completion.Result != ResultReconfigured {
		t.Fatalf("completion %+v", completion)
	}
	snapshot := h.stack.Snapshot(h.now)
	if snapshot.State != StateConfiguring || snapshot.Address != zeroIPv4 || snapshot.Gateway != zeroIPv4 ||
		snapshot.LeaseRemaining != 0 {
		t.Fatalf("after expiry %+v", snapshot)
	}
	messages := dhcpMessages(t, h.drain())
	if len(messages) != 1 || messages[0].kind != dhcpDiscover || messages[0].ciaddr != zeroIPv4 {
		t.Fatalf("after expiry %+v", messages)
	}
	if _, ok := h.stack.lookupNeighbor(gatewayIP, h.now); ok {
		t.Fatal("neighbors kept after expiry")
	}
}

func TestDHCPRebindAcceptsAnyServer(t *testing.T) {
	h, requested := boundHarness(t)
	h.runUntil(requested + testLease*second/8*7)
	frames := h.drain()
	rebind := onlyClient(t, frames[len(frames)-1:])
	other := IPv4{10, 0, 2, 4}
	options := leaseOptions(dhcpAck)
	options[1] = option{optionServerID, other[:]}
	h.receive(dhcpFrame(replyFor(rebind, hostIP, options).bytes()))
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.LeaseRemaining != testLease {
		t.Fatalf("after rebind %+v", snapshot)
	}
	if h.stack.dhcp.lease.server != other || h.stack.dhcp.phase != dhcpBound {
		t.Fatal("rebinding server not adopted")
	}
}

func TestDHCPRenewIgnoresOtherServer(t *testing.T) {
	h, requested := boundHarness(t)
	h.runUntil(requested + testLease*second/2)
	renew := onlyClient(t, h.drain())
	options := leaseOptions(dhcpAck)
	options[1] = option{optionServerID, []byte{10, 0, 2, 4}}
	h.receive(dhcpFrameTo(hostIP, replyFor(renew, hostIP, options).bytes()))
	if h.stack.dhcp.phase != dhcpRenewing {
		t.Fatal("ACK from another server accepted while renewing")
	}
}

func TestDHCPNak(t *testing.T) {
	nak := []option{{optionMessageType, []byte{dhcpNak}}, {optionServerID, serverIP[:]}}

	h, discover := dhcpHarness(t)
	request := offerAndAck(h, discover, nak)
	restart := onlyClient(t, h.drain())
	if restart.kind != dhcpDiscover || restart.xid == request.xid {
		t.Fatalf("after NAK %+v", restart)
	}

	h, requested := boundHarness(t)
	h.runUntil(requested + testLease*second/2)
	renew := onlyClient(t, h.drain())
	h.stack.Echo(0, peerIP, 1, MaxEchoTimeout, h.now)
	h.drain()
	h.receive(dhcpFrameTo(hostIP, replyFor(renew, hostIP, nak).bytes()))
	if completion := h.completion(); completion.Result != ResultReconfigured {
		t.Fatalf("completion %+v", completion)
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateConfiguring || snapshot.Address != zeroIPv4 {
		t.Fatalf("after NAK %+v", snapshot)
	}
	if discover := onlyClient(t, h.drain()); discover.kind != dhcpDiscover {
		t.Fatalf("after NAK %+v", discover)
	}
}

func TestDHCPRenewWithNewAddressDropsLease(t *testing.T) {
	h, requested := boundHarness(t)
	h.runUntil(requested + testLease*second/2)
	renew := onlyClient(t, h.drain())
	h.receive(dhcpFrameTo(hostIP, replyFor(renew, IPv4{10, 0, 2, 16}, leaseOptions(dhcpAck)).bytes()))
	if snapshot := h.stack.Snapshot(h.now); snapshot.Address != zeroIPv4 || snapshot.State != StateConfiguring {
		t.Fatalf("snapshot %+v", snapshot)
	}
}

func TestDHCPConflictDeclines(t *testing.T) {
	h, discover := dhcpHarness(t)
	offerAndAck(h, discover, leaseOptions(dhcpAck))
	h.drain()
	h.advance(200 * millisecond)
	h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: hostIP,
		targetMAC: hostMAC}))
	decline := onlyClient(t, h.drain())
	if decline.kind != dhcpDecline || decline.ciaddr != zeroIPv4 ||
		string(decline.options[optionRequestedIP]) != string(hostIP[:]) ||
		string(decline.options[optionServerID]) != string(serverIP[:]) ||
		decline.frame.ip.destination != broadcastIPv4 {
		t.Fatalf("decline %+v", decline)
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateConfiguring || snapshot.Address != zeroIPv4 {
		t.Fatalf("after conflict %+v", snapshot)
	}
	h.runUntil(h.now + dhcpDeclineWait - 1)
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("restarted before the decline wait")
	}
	h.advance(1)
	if restart := onlyClient(t, h.drain()); restart.kind != dhcpDiscover || restart.xid == discover.xid {
		t.Fatalf("restart %+v", restart)
	}
}

func TestDHCPRetransmissionBackoff(t *testing.T) {
	h, discover := dhcpHarness(t)
	last := h.now
	for _, base := range []uint64{4, 8, 16, 32, 64, 64, 64} {
		deadline, ok := h.stack.Deadline()
		if !ok {
			t.Fatal("no retransmission scheduled")
		}
		wait := deadline - last
		if wait < (base-1)*second || wait > (base+1)*second {
			t.Fatalf("base %d s: wait %d ns", base, wait)
		}
		h.runUntil(deadline)
		again := onlyClient(t, h.drain())
		if again.kind != dhcpDiscover || again.xid != discover.xid {
			t.Fatalf("retransmission %+v", again)
		}
		last = h.now
	}
	if again := h.stack.Snapshot(h.now); again.State != StateConfiguring {
		t.Fatalf("state %d", again.State)
	}
}

func TestDHCPRequestRetriesThenRediscovers(t *testing.T) {
	h, discover := dhcpHarness(t)
	h.receive(dhcpFrame(replyFor(discover, hostIP, leaseOptions(dhcpOffer)).bytes()))
	first := onlyClient(t, h.drain())
	for retry := 0; retry < dhcpRequestRetries; retry++ {
		deadline, _ := h.stack.Deadline()
		h.runUntil(deadline)
		if again := onlyClient(t, h.drain()); again.kind != dhcpRequest || again.xid != first.xid {
			t.Fatalf("retry %d: %+v", retry, again)
		}
	}
	deadline, _ := h.stack.Deadline()
	h.runUntil(deadline)
	if restart := onlyClient(t, h.drain()); restart.kind != dhcpDiscover || restart.xid == first.xid {
		t.Fatalf("restart %+v", restart)
	}
}

func TestDHCPOfferValidation(t *testing.T) {
	base := func(change func([]option) []option) []byte {
		return encodeOptions(change(leaseOptions(dhcpOffer)))
	}
	replace := func(code uint8, value []byte) func([]option) []option {
		return func(options []option) []option {
			for index := range options {
				if options[index].code == code {
					options[index].value = value
					return options
				}
			}
			return append(options, option{code, value})
		}
	}
	remove := func(code uint8) func([]option) []option {
		return func(options []option) []option {
			var kept []option
			for _, item := range options {
				if item.code != code {
					kept = append(kept, item)
				}
			}
			return kept
		}
	}
	add := func(code uint8, value []byte) func([]option) []option {
		return func(options []option) []option { return append(options, option{code, value}) }
	}
	good := func(reply *serverReply) {}
	for _, test := range []struct {
		name     string
		change   func(*serverReply)
		accepted bool
	}{
		{"good", good, true},
		{"no router", func(r *serverReply) { r.options = base(remove(optionRouter)) }, true},
		{"several routers", func(r *serverReply) { r.options = base(replace(optionRouter, []byte{10, 0, 2, 2, 10, 0, 2, 3})) }, true},
		{"explicit T1 and T2", func(r *serverReply) {
			r.options = base(func(o []option) []option {
				return append(o, option{optionRenewalTime, u32(30)}, option{optionRebindTime, u32(60)})
			})
		}, true},
		{"pads", func(r *serverReply) {
			r.options = append([]byte{0, 0, 0}, base(func(o []option) []option { return o })...)
		}, true},
		{"unknown option", func(r *serverReply) { r.options = base(add(200, make([]byte, 255))) }, true},
		{"minimum lease", func(r *serverReply) { r.options = base(replace(optionLeaseTime, u32(minLeaseSeconds))) }, true},

		{"no server id", func(r *serverReply) { r.options = base(remove(optionServerID)) }, false},
		{"broadcast server id", func(r *serverReply) { r.options = base(replace(optionServerID, broadcastIPv4[:])) }, false},
		{"no mask", func(r *serverReply) { r.options = base(remove(optionSubnetMask)) }, false},
		{"no lease", func(r *serverReply) { r.options = base(remove(optionLeaseTime)) }, false},
		{"no message type", func(r *serverReply) { r.options = base(remove(optionMessageType)) }, false},
		{"ack instead of offer", func(r *serverReply) { r.options = base(replace(optionMessageType, []byte{dhcpAck})) }, false},
		{"infinite lease", func(r *serverReply) { r.options = base(replace(optionLeaseTime, u32(infiniteLease))) }, false},
		{"short lease", func(r *serverReply) { r.options = base(replace(optionLeaseTime, u32(minLeaseSeconds-1))) }, false},
		{"zero lease", func(r *serverReply) { r.options = base(replace(optionLeaseTime, u32(0))) }, false},
		{"mask not contiguous", func(r *serverReply) { r.options = base(replace(optionSubnetMask, []byte{255, 0, 255, 0})) }, false},
		{"mask 31", func(r *serverReply) { r.options = base(replace(optionSubnetMask, []byte{255, 255, 255, 254})) }, false},
		{"mask zero", func(r *serverReply) { r.options = base(replace(optionSubnetMask, []byte{0, 0, 0, 0})) }, false},
		{"mask length", func(r *serverReply) { r.options = base(replace(optionSubnetMask, []byte{255, 255, 255})) }, false},
		{"router outside prefix", func(r *serverReply) { r.options = base(replace(optionRouter, []byte{10, 0, 3, 1})) }, false},
		{"router is offered address", func(r *serverReply) { r.options = base(replace(optionRouter, hostIP[:])) }, false},
		{"router length", func(r *serverReply) { r.options = base(replace(optionRouter, []byte{10, 0, 2, 2, 1})) }, false},
		{"T1 after T2", func(r *serverReply) {
			r.options = base(func(o []option) []option {
				return append(o, option{optionRenewalTime, u32(70)}, option{optionRebindTime, u32(60)})
			})
		}, false},
		{"T1 equals T2", func(r *serverReply) {
			r.options = base(func(o []option) []option {
				return append(o, option{optionRenewalTime, u32(60)}, option{optionRebindTime, u32(60)})
			})
		}, false},
		{"T2 at lease", func(r *serverReply) { r.options = base(add(optionRebindTime, u32(testLease))) }, false},
		{"T1 past default T2", func(r *serverReply) { r.options = base(add(optionRenewalTime, u32(110))) }, false},
		{"T1 zero", func(r *serverReply) { r.options = base(add(optionRenewalTime, u32(0))) }, false},
		{"duplicate option", func(r *serverReply) { r.options = base(add(optionLeaseTime, u32(testLease))) }, false},
		{"zero yiaddr", func(r *serverReply) { r.yiaddr = IPv4{} }, false},
		{"network yiaddr", func(r *serverReply) { r.yiaddr = IPv4{10, 0, 2, 0} }, false},
		{"broadcast yiaddr", func(r *serverReply) { r.yiaddr = IPv4{10, 0, 2, 255} }, false},
		{"multicast yiaddr", func(r *serverReply) { r.yiaddr = IPv4{224, 0, 2, 15} }, false},
		{"wrong xid", func(r *serverReply) { r.xid++ }, false},
		{"wrong chaddr", func(r *serverReply) { r.chaddr = peerMAC }, false},
		{"request op", func(r *serverReply) { r.op = bootRequest }, false},
		{"htype", func(r *serverReply) { r.htype = 6 }, false},
		{"hlen", func(r *serverReply) { r.hlen = 16 }, false},
		{"cookie", func(r *serverReply) { r.cookie = 0x63825364 }, false},
		{"no end", func(r *serverReply) { r.options = r.options[:len(r.options)-1] }, false},
		{"option overruns", func(r *serverReply) { r.options = []byte{optionMessageType, 1, dhcpOffer, optionLeaseTime, 200, 0} }, false},
		{"length byte missing", func(r *serverReply) { r.options = []byte{optionMessageType, 1, dhcpOffer, optionLeaseTime} }, false},
		{"no options", func(r *serverReply) { r.options = nil }, false},
		{"overload value", func(r *serverReply) { r.options = base(add(optionOverload, []byte{4})) }, false},
		{"overload length", func(r *serverReply) { r.options = base(add(optionOverload, []byte{1, 1})) }, false},
	} {
		h, discover := dhcpHarness(t)
		reply := replyFor(discover, hostIP, leaseOptions(dhcpOffer))
		test.change(&reply)
		before := h.stack.Snapshot(h.now)
		h.receive(dhcpFrame(reply.bytes()))
		frames := h.drain()
		if test.accepted {
			if len(frames) != 1 || parseClient(t, frames[0]).kind != dhcpRequest {
				t.Errorf("%s: offer refused", test.name)
			}
			continue
		}
		after := h.stack.Snapshot(h.now)
		if len(frames) != 0 || h.stack.dhcp.phase != dhcpSelecting {
			t.Errorf("%s: offer accepted", test.name)
		}
		if after.Drop+after.Malformed != before.Drop+before.Malformed+1 {
			t.Errorf("%s: not counted", test.name)
		}
	}
}

func TestDHCPOptionOverload(t *testing.T) {
	all := leaseOptions(dhcpOffer)
	for _, test := range []struct {
		name     string
		options  []option
		file     []option
		sname    []option
		accepted bool
	}{
		{"file", append(all[:1:1], option{optionOverload, []byte{overloadFile}}), all[1:], nil, true},
		{"sname", append(all[:1:1], option{optionOverload, []byte{overloadSname}}), nil, all[1:4], false},
		{"both", append(all[:1:1], option{optionOverload, []byte{overloadBoth}}), all[1:3], all[3:5], true},
		{"file not flagged", all[:1], all[1:], nil, false},
		{"overload inside file", append(all[:1:1], option{optionOverload, []byte{overloadFile}}),
			append(all[1:5:5], option{optionOverload, []byte{overloadSname}}), nil, false},
	} {
		h, discover := dhcpHarness(t)
		reply := replyFor(discover, hostIP, test.options)
		if test.file != nil {
			reply.file = encodeOptions(test.file)
		}
		if test.sname != nil {
			reply.sname = encodeOptions(test.sname)
		}
		if len(reply.file) > dhcpFileSize || len(reply.sname) > dhcpSnameSize {
			t.Fatalf("%s: test options too long", test.name)
		}
		h.receive(dhcpFrame(reply.bytes()))
		accepted := len(h.drain()) == 1
		if accepted != test.accepted {
			t.Errorf("%s: accepted %v", test.name, accepted)
		}
	}
	// An area that is all option bytes with no End must be refused, not
	// read past.
	h, discover := dhcpHarness(t)
	reply := replyFor(discover, hostIP, append(all[:1:1], option{optionOverload, []byte{overloadFile}}))
	reply.file = make([]byte, dhcpFileSize)
	for index := range reply.file {
		reply.file[index] = 6
	}
	h.receive(dhcpFrame(reply.bytes()))
	if len(h.drain()) != 0 {
		t.Fatal("unterminated file area accepted")
	}
}

func TestDHCPRouterInAckMustStayInPrefix(t *testing.T) {
	h, discover := dhcpHarness(t)
	options := leaseOptions(dhcpAck)
	options[3] = option{optionRouter, []byte{192, 0, 2, 1}}
	offerAndAck(h, discover, options)
	if frames := h.drain(); len(frames) != 0 || h.stack.dhcp.phase != dhcpRequesting {
		t.Fatal("ACK with a router outside the prefix accepted")
	}
}

func TestDHCPAckForOtherAddressIgnored(t *testing.T) {
	h, discover := dhcpHarness(t)
	h.receive(dhcpFrame(replyFor(discover, hostIP, leaseOptions(dhcpOffer)).bytes()))
	request := onlyClient(t, h.drain())
	h.receive(dhcpFrame(replyFor(request, IPv4{10, 0, 2, 16}, leaseOptions(dhcpAck)).bytes()))
	if h.stack.dhcp.phase != dhcpRequesting || len(h.drain()) != 0 {
		t.Fatal("ACK for another address accepted")
	}
	other := leaseOptions(dhcpAck)
	other[1] = option{optionServerID, []byte{10, 0, 2, 4}}
	h.receive(dhcpFrame(replyFor(request, hostIP, other).bytes()))
	if h.stack.dhcp.phase != dhcpRequesting {
		t.Fatal("ACK from another server accepted")
	}
}

func TestDHCPDatagramFiltering(t *testing.T) {
	h, discover := dhcpHarness(t)
	payload := replyFor(discover, hostIP, leaseOptions(dhcpOffer)).bytes()
	for name, frame := range map[string][]byte{
		"wrong source port": ipv4Frame(broadcastMAC, serverMAC, serverIP, broadcastIPv4, protocolUDP,
			udpBytes(serverIP, broadcastIPv4, 68, portClient, payload)),
		"wrong destination port": ipv4Frame(broadcastMAC, serverMAC, serverIP, broadcastIPv4, protocolUDP,
			udpBytes(serverIP, broadcastIPv4, portServer, 69, payload)),
		"zero source": ipv4Frame(broadcastMAC, serverMAC, IPv4{}, broadcastIPv4, protocolUDP,
			udpBytes(IPv4{}, broadcastIPv4, portServer, portClient, payload)),
		"multicast destination": ipv4Frame(broadcastMAC, serverMAC, serverIP, IPv4{224, 0, 0, 1}, protocolUDP,
			udpBytes(serverIP, IPv4{224, 0, 0, 1}, portServer, portClient, payload)),
		"bad UDP checksum": func() []byte {
			frame := dhcpFrame(payload)
			frame[len(frame)-1] ^= 0xff
			return frame
		}(),
		"other protocol": ipv4Frame(broadcastMAC, serverMAC, serverIP, broadcastIPv4, 6, payload),
	} {
		h.receive(frame)
		if len(h.drain()) != 0 {
			t.Errorf("%s: answered", name)
		}
	}
	// A server that ignores the broadcast flag and unicasts to the offered
	// address is still heard.
	h.receive(dhcpFrameTo(hostIP, payload))
	if request := onlyClient(t, h.drain()); request.kind != dhcpRequest {
		t.Fatalf("unicast offer %+v", request)
	}

	// Static mode never listens to DHCP.
	h = newHarness(t)
	h.readyStatic(staticConfig)
	before := h.stack.Snapshot(h.now).Drop
	h.receive(dhcpFrame(payload))
	if h.stack.Snapshot(h.now).Drop != before+1 || len(h.drain()) != 0 {
		t.Fatal("static stack processed a DHCP broadcast")
	}
}

func TestDHCPLateProbeGoesStraightToRenew(t *testing.T) {
	h, discover := dhcpHarness(t)
	options := append(leaseOptions(dhcpAck), option{optionRenewalTime, u32(1)}, option{optionRebindTime, u32(2)})
	offerAndAck(h, discover, options)
	h.drain()
	// T1 passes during the conflict probe, so binding starts a renew at once.
	h.runUntil(h.now + second)
	messages := dhcpMessages(t, h.drain())
	if len(messages) != 1 || messages[0].kind != dhcpRequest || messages[0].ciaddr != hostIP {
		t.Fatalf("messages %+v", messages)
	}
	if h.stack.dhcp.phase != dhcpRenewing {
		t.Fatalf("phase %d", h.stack.dhcp.phase)
	}
}
