package network

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func (h *harness) open(owner int) EndpointID {
	h.t.Helper()
	id, result := h.stack.UDPOpen(owner)
	if result != ResultOK || id == 0 {
		h.t.Fatalf("open: %d", result)
	}
	return id
}

func (h *harness) bind(owner int, id EndpointID, address IPv4, port uint16) uint16 {
	h.t.Helper()
	_, bound, result := h.stack.UDPBind(owner, id, address, port)
	if result != ResultOK {
		h.t.Fatalf("bind %v:%d: %d", address, port, result)
	}
	return bound
}

// take expects one queued datagram on id and returns it with its payload.
func (h *harness) take(owner int, id EndpointID) (UDPReceived, []byte) {
	h.t.Helper()
	var buffer [UDPPayloadCapacity]byte
	received, ok := h.stack.UDPReceive(owner, id, buffer[:])
	if !ok {
		h.t.Fatal("no datagram queued")
	}
	return received, append([]byte(nil), buffer[:received.Length]...)
}

func (h *harness) empty(owner int, id EndpointID) {
	h.t.Helper()
	if count, result := h.stack.UDPQueued(owner, id); result != ResultOK || count != 0 {
		h.t.Fatalf("queued %d result %d, want none", count, result)
	}
}

// udpFrame is a datagram from the link to the host.
func udpFrame(from IPv4, mac MAC, sourcePort, destinationPort uint16, payload []byte) []byte {
	return ipv4Frame(hostMAC, mac, from, hostIP, protocolUDP, udpBytes(from, hostIP, sourcePort, destinationPort, payload))
}

func TestUDPBindExplicitAndEphemeral(t *testing.T) {
	h := readyWithPeer(t)
	first := h.open(0)
	if port := h.bind(0, first, IPv4{}, 9000); port != 9000 {
		t.Fatalf("port %d", port)
	}
	second := h.open(0)
	port := h.bind(0, second, hostIP, 0)
	if port < 49152 {
		t.Fatalf("ephemeral port %d", port)
	}
	third := h.open(0)
	if _, _, result := h.stack.UDPBind(0, third, LoopbackAddress, port); result != ResultOK {
		t.Fatalf("same port on another address: %d", result)
	}
	if _, _, result := h.stack.UDPBind(0, first, IPv4{}, 9001); result != ResultInvalidArgument {
		t.Fatalf("second bind of one endpoint: %d", result)
	}
}

func TestUDPBindConflicts(t *testing.T) {
	h := readyWithPeer(t)
	wildcard := h.open(0)
	h.bind(0, wildcard, IPv4{}, 9000)
	for _, address := range []IPv4{{}, hostIP, LoopbackAddress} {
		id := h.open(1)
		if _, _, result := h.stack.UDPBind(1, id, address, 9000); result != ResultAddressInUse {
			t.Fatalf("%v:9000 beside the wildcard: %d", address, result)
		}
		h.stack.UDPClose(1, id)
	}
	specific := h.open(0)
	h.bind(0, specific, hostIP, 9001)
	same := h.open(1)
	if _, _, result := h.stack.UDPBind(1, same, hostIP, 9001); result != ResultAddressInUse {
		t.Fatalf("identical bind: %d", result)
	}
	if _, _, result := h.stack.UDPBind(1, same, IPv4{}, 9001); result != ResultAddressInUse {
		t.Fatalf("wildcard over a specific bind: %d", result)
	}
	if stats := h.stack.UDPStats(); stats.BindFail != 5 {
		t.Fatalf("bind failures %d", stats.BindFail)
	}
}

func TestUDPBindRefusesForeignAddresses(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	for _, address := range []IPv4{peerIP, broadcastIPv4, {224, 0, 0, 1}, {10, 0, 2, 255}, {127, 0, 0, 0}, {127, 255, 255, 255}} {
		if _, _, result := h.stack.UDPBind(0, id, address, 9000); result != ResultInvalidArgument {
			t.Errorf("bind %v: %d", address, result)
		}
	}
	other := IPv4{127, 1, 2, 3}
	if address, _, result := h.stack.UDPBind(0, id, other, 9000); result != ResultOK || address != other {
		t.Fatalf("bind %v: %v %d", other, address, result)
	}
}

func TestUDPDHCPPortReserved(t *testing.T) {
	h := newHarness(t)
	id := h.open(0)
	if _, _, result := h.stack.UDPBind(0, id, IPv4{}, portClient); result != ResultAddressInUse {
		t.Fatalf("port 68 in DHCP mode: %d", result)
	}
	h.stack.Configure(staticConfig, h.now)
	if _, _, result := h.stack.UDPBind(0, id, IPv4{}, portClient); result != ResultOK {
		t.Fatalf("port 68 in static mode: %d", result)
	}
}

func TestUDPEndpointExhaustionAndRelease(t *testing.T) {
	h := newHarness(t)
	var ids []EndpointID
	for owner := 0; owner < MaxUDPEndpoints/MaxUDPSessionEndpoints; owner++ {
		for count := 0; count < MaxUDPSessionEndpoints; count++ {
			ids = append(ids, h.open(owner))
		}
	}
	if _, result := h.stack.UDPOpen(MaxUDPSessions - 1); result != ResultBusy {
		t.Fatalf("open past the table: %d", result)
	}
	if _, result := h.stack.UDPOpen(0); result != ResultBusy {
		t.Fatalf("open past the session share: %d", result)
	}
	if stats := h.stack.UDPStats(); stats.Endpoints != MaxUDPEndpoints {
		t.Fatalf("endpoints %d", stats.Endpoints)
	}
	bound := h.bind(0, ids[0], IPv4{}, 7000)
	h.stack.UDPClose(0, ids[0])
	again := h.open(0)
	if again.slot() != ids[0].slot() || again == ids[0] {
		t.Fatalf("reopened %#x after %#x", again, ids[0])
	}
	if h.bind(0, again, IPv4{}, bound) != bound {
		t.Fatal("closed endpoint kept its port")
	}
}

func TestUDPStaleAndForeignIDs(t *testing.T) {
	h := readyWithPeer(t)
	old := h.open(0)
	h.stack.UDPClose(0, old)
	fresh := h.open(0)
	if fresh.slot() != old.slot() {
		t.Fatal("slot not reused")
	}
	for _, test := range []struct {
		owner int
		id    EndpointID
	}{{0, old}, {1, fresh}, {0, 0}, {0, EndpointID(MaxUDPEndpoints)}, {-1, fresh}, {MaxUDPSessions, fresh}} {
		if result := h.stack.UDPClose(test.owner, test.id); result != ResultInvalidArgument {
			t.Errorf("close %d/%#x: %d", test.owner, test.id, result)
		}
		if result := h.stack.UDPSendTo(test.owner, test.id, peerIP, 9, nil, h.now); result != ResultInvalidArgument {
			t.Errorf("send %d/%#x: %d", test.owner, test.id, result)
		}
		if _, _, result := h.stack.UDPBind(test.owner, test.id, IPv4{}, 0); result != ResultInvalidArgument {
			t.Errorf("bind %d/%#x: %d", test.owner, test.id, result)
		}
	}
	if _, result := h.stack.UDPQueued(0, fresh); result != ResultInvalidArgument {
		t.Fatal("unbound endpoint reported a queue")
	}
}

func TestUDPEphemeralPortsAvoidBound(t *testing.T) {
	h := newHarness(t)
	tuning := DefaultTuning()
	tuning.UDP.EphemeralMin, tuning.UDP.EphemeralMax = 50000, 50003
	h.stack.tuning = tuning
	seen := map[uint16]bool{}
	for count := 0; count < 4; count++ {
		port := h.bind(0, h.open(0), IPv4{}, 0)
		if port < 50000 || port > 50003 || seen[port] {
			t.Fatalf("port %d, seen %v", port, seen)
		}
		seen[port] = true
	}
	if _, _, result := h.stack.UDPBind(1, h.open(1), IPv4{}, 0); result != ResultBusy {
		t.Fatalf("exhausted range: %d", result)
	}
}

func TestUDPSendWithCachedNeighbor(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	if result := h.stack.UDPSendTo(0, id, peerIP, 7, []byte("ping"), h.now); result != ResultOK {
		t.Fatalf("send %d", result)
	}
	sent := only(t, h.drain())
	if sent.ether.destination != peerMAC || sent.ip.source != hostIP || sent.ip.destination != peerIP ||
		sent.udp.destinationPort != 7 || sent.udp.sourcePort < 49152 || string(sent.udp.payload) != "ping" {
		t.Fatalf("sent %+v", sent)
	}
	if stats := h.stack.UDPStats(); stats.TX != 1 {
		t.Fatalf("tx %d", stats.TX)
	}
	// The send bound the endpoint to the wildcard and that port.
	h.receive(udpFrame(peerIP, peerMAC, 7, sent.udp.sourcePort, []byte("pong")))
	received, payload := h.take(0, id)
	if string(payload) != "pong" || received.Source != peerIP || received.SourcePort != 7 || received.Destination != hostIP {
		t.Fatalf("received %+v %q", received, payload)
	}
}

func TestUDPSendResolvesNextHop(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	id := h.open(0)
	if result := h.stack.UDPSendTo(0, id, remoteIP, 53, []byte("query"), h.now); result != ResultOK {
		t.Fatalf("send %d", result)
	}
	request := only(t, h.drain())
	if request.arp.operation != arpRequest || request.arp.targetIP != gatewayIP {
		t.Fatalf("expected ARP for the gateway, got %+v", request)
	}
	h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: gatewayMAC, senderIP: gatewayIP,
		targetMAC: hostMAC, targetIP: hostIP}))
	sent := only(t, h.drain())
	if sent.ether.destination != gatewayMAC || sent.ip.destination != remoteIP || string(sent.udp.payload) != "query" {
		t.Fatalf("sent %+v", sent)
	}
}

func TestUDPSendDroppedWhenARPFails(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	id := h.open(0)
	if result := h.stack.UDPSendTo(0, id, peerIP, 9, []byte("x"), h.now); result != ResultOK {
		t.Fatalf("send %d", result)
	}
	drops := h.stack.Snapshot(h.now).Drop
	h.runUntil(h.now + 10*second)
	for _, frame := range h.drain() {
		if decode(t, frame).ip.protocol == protocolUDP {
			t.Fatal("datagram sent without a neighbor")
		}
	}
	if h.stack.Snapshot(h.now).Drop != drops+1 || h.stack.freePending() != 0 {
		t.Fatal("pending datagram not dropped")
	}
}

func TestUDPSendFailures(t *testing.T) {
	h := newHarness(t)
	id := h.open(0)
	for _, test := range []struct {
		name        string
		destination IPv4
		port        uint16
		want        Result
	}{
		{"port zero", peerIP, 0, ResultInvalidArgument},
		{"broadcast", broadcastIPv4, 9, ResultInvalidArgument},
		{"multicast", IPv4{224, 0, 0, 251}, 5353, ResultInvalidArgument},
		{"zero", IPv4{}, 9, ResultInvalidArgument},
		{"loopback network", IPv4{127, 0, 0, 0}, 9, ResultInvalidArgument},
		{"link down", peerIP, 9, ResultDown},
	} {
		if result := h.stack.UDPSendTo(0, id, test.destination, test.port, nil, h.now); result != test.want {
			t.Errorf("%s: %d, want %d", test.name, result, test.want)
		}
	}
	h.stack.Configure(staticConfig, h.now)
	h.stack.LinkUp(hostMAC, MTU, h.now)
	if result := h.stack.UDPSendTo(0, id, peerIP, 9, nil, h.now); result != ResultNoAddress {
		t.Fatalf("before the claim: %d", result)
	}
	h.claim()
	h.finishAnnouncements()
	h.drain()
	if result := h.stack.UDPSendTo(0, id, IPv4{10, 0, 2, 255}, 9, nil, h.now); result != ResultInvalidArgument {
		t.Fatalf("subnet broadcast: %d", result)
	}
	noGateway := staticConfig
	noGateway.Gateway = IPv4{}
	h.stack.Configure(noGateway, h.now)
	if result := h.stack.UDPSendTo(0, id, remoteIP, 9, nil, h.now); result != ResultNoRoute {
		t.Fatalf("off-link without a gateway: %d", result)
	}
	if _, result := h.stack.UDPQueued(0, id); result != ResultInvalidArgument {
		t.Fatal("refused sends bound the endpoint")
	}
}

func TestUDPPayloadLimits(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	limit := int(MTU) - ipv4HeaderSize - udpHeaderSize
	if limit != 1472 {
		t.Fatalf("limit %d", limit)
	}
	if result := h.stack.UDPSendTo(0, id, peerIP, 9, make([]byte, limit), h.now); result != ResultOK {
		t.Fatalf("largest unfragmented payload: %d", result)
	}
	if sent := only(t, h.drain()); len(sent.udp.payload) != limit {
		t.Fatalf("sent %d bytes", len(sent.udp.payload))
	}
	if result := h.stack.UDPSendTo(0, id, peerIP, 9, make([]byte, limit+1), h.now); result != ResultInvalidArgument {
		t.Fatalf("one byte over: %d", result)
	}
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("oversized payload framed")
	}
	if result := h.stack.UDPSendTo(0, id, LoopbackAddress, 9, make([]byte, UDPPayloadCapacity), h.now); result != ResultOK {
		t.Fatalf("loopback at the payload capacity: %d", result)
	}
	if result := h.stack.UDPSendTo(0, id, LoopbackAddress, 9, make([]byte, UDPPayloadCapacity+1), h.now); result != ResultInvalidArgument {
		t.Fatalf("loopback past the capacity: %d", result)
	}
}

func TestUDPZeroLength(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	port := h.bind(0, id, IPv4{}, 9000)
	if result := h.stack.UDPSendTo(0, id, peerIP, 9, nil, h.now); result != ResultOK {
		t.Fatalf("send %d", result)
	}
	if sent := only(t, h.drain()); len(sent.udp.payload) != 0 || binary.BigEndian.Uint16(sent.ip.payload[4:6]) != udpHeaderSize {
		t.Fatalf("sent %+v", sent)
	}
	h.receive(udpFrame(peerIP, peerMAC, 9, port, nil))
	if received, _ := h.take(0, id); received.Length != 0 || received.Source != peerIP {
		t.Fatalf("received %+v", received)
	}
}

func TestUDPChecksum(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	port := h.bind(0, id, IPv4{}, 9000)
	h.stack.UDPSendTo(0, id, peerIP, 9, []byte("abc"), h.now)
	sent := only(t, h.drain())
	if sum := pseudoHeaderSum(sent.ip.source, sent.ip.destination, protocolUDP, len(sent.ip.payload)); checksumFold(checksumAdd(sum, sent.ip.payload)) != 0 {
		t.Fatal("transmit checksum does not verify")
	}
	good := udpFrame(peerIP, peerMAC, 9, port, []byte("one"))
	h.receive(good)
	h.take(0, id)
	zero := udpFrame(peerIP, peerMAC, 9, port, []byte("two"))
	zero[HeaderSize+ipv4HeaderSize+6], zero[HeaderSize+ipv4HeaderSize+7] = 0, 0
	h.receive(zero)
	if _, payload := h.take(0, id); string(payload) != "two" {
		t.Fatalf("checksum 0 payload %q", payload)
	}
	bad := udpFrame(peerIP, peerMAC, 9, port, []byte("bad"))
	bad[len(bad)-1] ^= 0xff
	h.receive(bad)
	h.empty(0, id)
	if stats := h.stack.UDPStats(); stats.DropChecksum != 1 || stats.RX != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

// TestUDPChecksumZeroIsSentAsOnes finds a payload whose checksum folds to
// zero and checks it goes out as 0xffff.
func TestUDPChecksumZeroIsSentAsOnes(t *testing.T) {
	payload := []byte{0, 0}
	datagram := make([]byte, udpHeaderSize+2)
	for value := 0; value < 1<<16; value++ {
		binary.BigEndian.PutUint16(payload, uint16(value))
		copy(datagram[udpHeaderSize:], payload)
		datagram[6], datagram[7] = 0, 0
		binary.BigEndian.PutUint16(datagram[0:2], 1000)
		binary.BigEndian.PutUint16(datagram[2:4], 2000)
		binary.BigEndian.PutUint16(datagram[4:6], uint16(len(datagram)))
		sum := pseudoHeaderSum(hostIP, peerIP, protocolUDP, len(datagram))
		if checksumFold(checksumAdd(sum, datagram)) != 0 {
			continue
		}
		putUDP(datagram, hostIP, peerIP, 1000, 2000, 2)
		if binary.BigEndian.Uint16(datagram[6:8]) != 0xffff {
			t.Fatalf("computed zero sent as %#x", binary.BigEndian.Uint16(datagram[6:8]))
		}
		return
	}
	t.Fatal("no payload folds to zero")
}

func TestUDPReceiveDemux(t *testing.T) {
	h := readyWithPeer(t)
	wildcard := h.open(0)
	h.bind(0, wildcard, IPv4{}, 9000)
	specific := h.open(1)
	h.bind(1, specific, hostIP, 9001)
	loop := h.open(1)
	h.bind(1, loop, LoopbackAddress, 9001)
	h.receive(udpFrame(peerIP, peerMAC, 1234, 9000, []byte("w")))
	h.receive(udpFrame(peerIP, peerMAC, 1234, 9001, []byte("s")))
	h.receive(udpFrame(peerIP, peerMAC, 1234, 9002, []byte("none")))
	if _, payload := h.take(0, wildcard); string(payload) != "w" {
		t.Fatalf("wildcard got %q", payload)
	}
	if _, payload := h.take(1, specific); string(payload) != "s" {
		t.Fatalf("specific got %q", payload)
	}
	h.empty(1, loop)
	h.empty(0, wildcard)
	if stats := h.stack.UDPStats(); stats.DropNoEndpoint != 1 || stats.RX != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestUDPConnectedFiltersPeer(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	local, port, result := h.stack.UDPConnect(0, id, peerIP, 53, h.now)
	if result != ResultOK || local != hostIP || port < 49152 {
		t.Fatalf("connect %v:%d %d", local, port, result)
	}
	if result := h.stack.UDPSendTo(0, id, peerIP, 53, nil, h.now); result != ResultInvalidArgument {
		t.Fatalf("SendTo on a connected endpoint: %d", result)
	}
	if _, _, result := h.stack.UDPConnect(0, id, peerIP, 54, h.now); result != ResultInvalidArgument {
		t.Fatalf("second connect: %d", result)
	}
	if result := h.stack.UDPSend(0, id, []byte("q"), h.now); result != ResultOK {
		t.Fatalf("send %d", result)
	}
	if sent := only(t, h.drain()); sent.udp.sourcePort != port || sent.udp.destinationPort != 53 {
		t.Fatalf("sent %+v", sent)
	}
	h.receive(udpFrame(peerIP, peerMAC, 54, port, []byte("wrong port")))
	h.receive(udpFrame(gatewayIP, gatewayMAC, 53, port, []byte("wrong peer")))
	h.receive(udpFrame(peerIP, peerMAC, 53, port, []byte("answer")))
	received, payload := h.take(0, id)
	if string(payload) != "answer" || received.SourcePort != 53 {
		t.Fatalf("received %+v %q", received, payload)
	}
	h.empty(0, id)
	unconnected := h.open(0)
	if result := h.stack.UDPSend(0, unconnected, nil, h.now); result != ResultInvalidArgument {
		t.Fatalf("Send without a peer: %d", result)
	}
}

func TestUDPQueueOrderAndOverflow(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	port := h.bind(0, id, IPv4{}, 9000)
	depth := int(defaults.UDP.ReceiveQueue)
	for index := 0; index <= depth; index++ {
		h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte{byte(index)}))
	}
	if stats := h.stack.UDPStats(); stats.DropQueueFull != 1 || stats.RX != uint64(depth) {
		t.Fatalf("stats %+v", stats)
	}
	for index := 0; index < depth; index++ {
		if _, payload := h.take(0, id); !bytes.Equal(payload, []byte{byte(index)}) {
			t.Fatalf("datagram %d is %v", index, payload)
		}
	}
	h.empty(0, id)
}

func TestUDPSharedBuffersBoundAllQueues(t *testing.T) {
	h := readyWithPeer(t)
	var ports []uint16
	for owner := 0; owner < MaxUDPEndpoints/MaxUDPSessionEndpoints; owner++ {
		for count := 0; count < MaxUDPSessionEndpoints; count++ {
			ports = append(ports, h.bind(owner, h.open(owner), IPv4{}, 0))
		}
	}
	for _, port := range ports {
		h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte("a")))
		h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte("b")))
	}
	if stats := h.stack.UDPStats(); stats.RX != UDPReceiveBuffers || stats.DropQueueFull != uint64(2*len(ports)-UDPReceiveBuffers) {
		t.Fatalf("stats %+v", stats)
	}
}

func TestUDPSessionBufferShare(t *testing.T) {
	h := readyWithPeer(t)
	var ports []uint16
	for count := 0; count < MaxUDPSessionEndpoints; count++ {
		ports = append(ports, h.bind(0, h.open(0), IPv4{}, 0))
	}
	other := h.open(1)
	otherPort := h.bind(1, other, IPv4{}, 0)
	for _, port := range ports {
		for count := 0; count < int(defaults.UDP.ReceiveQueue); count++ {
			h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte("flood")))
		}
	}
	if stats := h.stack.UDPStats(); stats.RX != UDPSessionBuffers {
		t.Fatalf("one session queued %d datagrams", stats.RX)
	}
	h.receive(udpFrame(peerIP, peerMAC, 7, otherPort, []byte("still room")))
	if _, payload := h.take(1, other); string(payload) != "still room" {
		t.Fatalf("payload %q", payload)
	}
}

func TestUDPReceiveNeedsRoom(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	port := h.bind(0, id, IPv4{}, 9000)
	h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte("four")))
	var small [3]byte
	if _, ok := h.stack.UDPReceive(0, id, small[:]); ok {
		t.Fatal("datagram truncated into a short buffer")
	}
	if _, payload := h.take(0, id); string(payload) != "four" {
		t.Fatalf("payload %q", payload)
	}
	if _, ok := h.stack.UDPReceive(0, id, small[:]); ok {
		t.Fatal("delivered twice")
	}
}

func TestUDPLoopbackWithoutDevice(t *testing.T) {
	h := newHarness(t)
	server := h.open(0)
	h.bind(0, server, IPv4{127, 0, 0, 2}, 7000)
	client := h.open(1)
	for _, destination := range []IPv4{{127, 0, 0, 2}, {127, 0, 0, 1}} {
		if result := h.stack.UDPSendTo(1, client, destination, 7000, []byte("hi"), h.now); result != ResultOK {
			t.Fatalf("send to %v: %d", destination, result)
		}
	}
	received, payload := h.take(0, server)
	if string(payload) != "hi" || received.Source != LoopbackAddress || received.Destination != (IPv4{127, 0, 0, 2}) {
		t.Fatalf("received %+v %q", received, payload)
	}
	h.empty(0, server)
	if stats := h.stack.UDPStats(); stats.DropNoEndpoint != 1 {
		t.Fatalf("unbound loopback destination not dropped: %+v", stats)
	}
	if frames := h.drain(); len(frames) != 0 || h.stack.resolutions[0].used {
		t.Fatal("loopback used ARP or the link")
	}
	reply := received.SourcePort
	if result := h.stack.UDPSendTo(0, server, LoopbackAddress, reply, []byte("back"), h.now); result != ResultOK {
		t.Fatalf("reply %d", result)
	}
	if received, payload := h.take(1, client); string(payload) != "back" || received.Source != (IPv4{127, 0, 0, 2}) {
		t.Fatalf("reply %+v %q", received, payload)
	}
}

func TestUDPOwnAddressIsLocal(t *testing.T) {
	h := readyWithPeer(t)
	server := h.open(0)
	h.bind(0, server, hostIP, 7000)
	client := h.open(1)
	resolutions := h.stack.resolutions
	if result := h.stack.UDPSendTo(1, client, hostIP, 7000, []byte("self"), h.now); result != ResultOK {
		t.Fatalf("send %d", result)
	}
	if frames := h.drain(); len(frames) != 0 || h.stack.resolutions != resolutions {
		t.Fatal("own address went to ARP or the link")
	}
	if received, payload := h.take(0, server); string(payload) != "self" || received.Source != hostIP {
		t.Fatalf("received %+v %q", received, payload)
	}
}

func TestUDPFramesToOtherAddressesAreNotDelivered(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	port := h.bind(0, id, IPv4{}, 9000)
	broadcast := ipv4Frame(broadcastMAC, peerMAC, peerIP, broadcastIPv4, protocolUDP, udpBytes(peerIP, broadcastIPv4, 7, port, []byte("b")))
	h.receive(broadcast)
	looped := ipv4Frame(hostMAC, peerMAC, peerIP, LoopbackAddress, protocolUDP, udpBytes(peerIP, LoopbackAddress, 7, port, []byte("l")))
	h.receive(looped)
	h.empty(0, id)
}

func TestUDPReconfiguration(t *testing.T) {
	h := readyWithPeer(t)
	wildcard := h.open(0)
	h.bind(0, wildcard, IPv4{}, 9000)
	specific := h.open(0)
	h.bind(0, specific, hostIP, 9001)
	loop := h.open(0)
	h.bind(0, loop, LoopbackAddress, 9002)
	moved := staticConfig
	moved.Address = IPv4{10, 0, 2, 16}
	h.stack.Configure(moved, h.now)
	h.claim()
	h.finishAnnouncements()
	h.learn(peerIP, peerMAC)
	if result := h.stack.UDPSendTo(0, specific, peerIP, 9, nil, h.now); result != ResultReconfigured {
		t.Fatalf("send from a removed address: %d", result)
	}
	if result := h.stack.UDPSendTo(0, wildcard, peerIP, 9, nil, h.now); result != ResultOK {
		t.Fatalf("wildcard send: %d", result)
	}
	if sent := only(t, h.drain()); sent.ip.source != moved.Address {
		t.Fatalf("source %v", sent.ip.source)
	}
	h.stack.LinkDown(ResultDown, h.now)
	if result := h.stack.UDPSendTo(0, wildcard, LoopbackAddress, 9002, []byte("lo"), h.now); result != ResultOK {
		t.Fatalf("loopback with the link down: %d", result)
	}
	if _, payload := h.take(0, loop); string(payload) != "lo" {
		t.Fatalf("loopback payload %q", payload)
	}
}

func TestUDPCloseOwner(t *testing.T) {
	h := readyWithPeer(t)
	mine := h.open(0)
	port := h.bind(0, mine, IPv4{}, 9000)
	other := h.open(1)
	h.bind(1, other, IPv4{}, 9001)
	h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte("queued")))
	h.stack.UDPCloseOwner(0)
	if result := h.stack.UDPClose(0, mine); result != ResultInvalidArgument {
		t.Fatal("endpoint survived its owner")
	}
	for index := range h.stack.udpBuffers[:] {
		if h.stack.udpBuffers[index].used {
			t.Fatal("queued buffer kept")
		}
	}
	if stats := h.stack.UDPStats(); stats.Endpoints != 1 {
		t.Fatalf("endpoints %d", stats.Endpoints)
	}
	h.bind(1, h.open(1), hostIP, 9000)
}

func TestUDPDHCPTrafficIsNotStolen(t *testing.T) {
	h := newHarness(t)
	h.stack.Configure(staticConfig, h.now)
	id := h.open(0)
	h.bind(0, id, IPv4{}, portClient)
	h.stack.Configure(Config{Mode: ModeDHCP}, h.now)
	h.stack.LinkUp(hostMAC, MTU, h.now)
	h.drain()
	offer := ipv4Frame(broadcastMAC, gatewayMAC, gatewayIP, broadcastIPv4, protocolUDP,
		udpBytes(gatewayIP, broadcastIPv4, portServer, portClient, []byte("offer")))
	h.receive(offer)
	h.empty(0, id)
}

func TestUDPTuningValidation(t *testing.T) {
	base := DefaultTuning()
	for _, test := range []struct {
		name   string
		change func(*UDPTuning)
		want   Field
	}{
		{"empty port range", func(u *UDPTuning) { u.EphemeralMin, u.EphemeralMax = 60000, 59999 }, FieldUDPEphemeralMax},
		{"single port", func(u *UDPTuning) { u.EphemeralMin, u.EphemeralMax = 60000, 60000 }, FieldNone},
		{"reserved ports", func(u *UDPTuning) { u.EphemeralMin = 68 }, FieldUDPEphemeralMin},
		{"port zero", func(u *UDPTuning) { u.EphemeralMin = 0 }, FieldUDPEphemeralMin},
		{"session over total", func(u *UDPTuning) { u.MaxEndpoints, u.SessionEndpoints = 4, 5 }, FieldUDPSessionEndpoints},
		{"session equal total", func(u *UDPTuning) { u.MaxEndpoints, u.SessionEndpoints = 4, 4 }, FieldNone},
		{"endpoints over capacity", func(u *UDPTuning) { u.MaxEndpoints = MaxUDPEndpoints + 1 }, FieldUDPMaxEndpoints},
		{"session over capacity", func(u *UDPTuning) { u.SessionEndpoints = MaxUDPSessionEndpoints + 1 }, FieldUDPSessionEndpoints},
		{"queue over capacity", func(u *UDPTuning) { u.ReceiveQueue = UDPQueueCapacity + 1 }, FieldUDPReceiveQueue},
		{"queue zero", func(u *UDPTuning) { u.ReceiveQueue = 0 }, FieldUDPReceiveQueue},
		{"payload over capacity", func(u *UDPTuning) { u.MaxPayload = UDPPayloadCapacity + 1 }, FieldUDPMaxPayload},
		{"payload under DNS", func(u *UDPTuning) { u.MaxPayload = udpMinPayload - 1 }, FieldUDPMaxPayload},
		{"queue at capacity", func(u *UDPTuning) { u.ReceiveQueue = UDPQueueCapacity }, FieldNone},
	} {
		tuning := base
		test.change(&tuning.UDP)
		if got := tuning.Validate(); got != test.want {
			t.Errorf("%s: field %d, want %d", test.name, got, test.want)
		}
	}
}

func TestUDPTuningParse(t *testing.T) {
	values := storedDefaults
	values[FieldUDPEphemeralMin-FieldTTL] = 40000
	values[FieldUDPEphemeralMax-FieldTTL] = 40100
	values[FieldUDPReceiveQueue-FieldTTL] = 4
	values[FieldUDPMaxEndpoints-FieldTTL] = 16
	values[FieldUDPSessionEndpoints-FieldTTL] = 2
	values[FieldUDPMaxPayload-FieldTTL] = 1500
	tuning, field := ParseTuning(&values)
	want := UDPTuning{EphemeralMin: 40000, EphemeralMax: 40100, ReceiveQueue: 4, MaxEndpoints: 16, SessionEndpoints: 2, MaxPayload: 1500}
	if field != FieldNone || tuning.UDP != want {
		t.Fatalf("parsed %+v field %d", tuning.UDP, field)
	}
	values[FieldUDPEphemeralMin-FieldTTL] = 1 << 32
	if _, field := ParseTuning(&values); field != FieldUDPEphemeralMin {
		t.Fatalf("wrapping port accepted: %d", field)
	}
}

func TestUDPTuningRejectedAtomically(t *testing.T) {
	h := readyWithPeer(t)
	before := h.stack.Tuning()
	bad := DefaultTuning()
	bad.TTL = 99
	bad.UDP.ReceiveQueue = 2
	bad.UDP.MaxEndpoints, bad.UDP.SessionEndpoints = 2, 3
	if field := h.stack.Apply(staticConfig, bad, h.now); field != FieldUDPSessionEndpoints {
		t.Fatalf("field %d", field)
	}
	if h.stack.Tuning() != before {
		t.Fatal("a refused UDP limit changed the tuning")
	}
}

func TestUDPTuningReload(t *testing.T) {
	h := readyWithPeer(t)
	tuning := DefaultTuning()
	tuning.UDP.EphemeralMin, tuning.UDP.EphemeralMax = 40000, 40000
	tuning.UDP.MaxPayload = 600
	if field := h.stack.Apply(staticConfig, tuning, h.now); field != FieldNone {
		t.Fatalf("field %d", field)
	}
	if h.stack.Tuning().UDP != tuning.UDP {
		t.Fatal("tuning not applied")
	}
	id := h.open(0)
	if port := h.bind(0, id, IPv4{}, 0); port != 40000 {
		t.Fatalf("port %d outside the new range", port)
	}
	if result := h.stack.UDPSendTo(0, id, peerIP, 9, make([]byte, 601), h.now); result != ResultInvalidArgument {
		t.Fatalf("payload over the new limit: %d", result)
	}
	h.receive(udpFrame(peerIP, peerMAC, 7, 40000, make([]byte, 601)))
	h.empty(0, id)
	if stats := h.stack.UDPStats(); stats.DropTooLong != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

// TestUDPLoweredLimitsKeepWhatExists lowers every limit below current use:
// endpoints, ports and queued datagrams stay, and only new acquisitions see
// the new limits.
func TestUDPLoweredLimitsKeepWhatExists(t *testing.T) {
	h := readyWithPeer(t)
	ids := []EndpointID{h.open(0), h.open(0), h.open(1)}
	port := h.bind(0, ids[0], IPv4{}, 9000)
	ephemeral := h.bind(1, ids[2], IPv4{}, 0)
	for index := 0; index < 4; index++ {
		h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte{byte(index), 0, 0}))
	}
	h.receive(udpFrame(peerIP, peerMAC, 7, port, make([]byte, 1000)))
	tuning := DefaultTuning()
	tuning.UDP.MaxEndpoints, tuning.UDP.SessionEndpoints = 2, 1
	tuning.UDP.ReceiveQueue = 2
	tuning.UDP.MaxPayload = 512
	tuning.UDP.EphemeralMin, tuning.UDP.EphemeralMax = 2000, 2000
	if field := h.stack.Apply(staticConfig, tuning, h.now); field != FieldNone {
		t.Fatalf("field %d", field)
	}
	if _, result := h.stack.UDPOpen(2); result != ResultBusy {
		t.Fatalf("open over the lowered total: %d", result)
	}
	if stats := h.stack.UDPStats(); stats.Endpoints != 3 {
		t.Fatalf("endpoints %d", stats.Endpoints)
	}
	h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte("late")))
	if stats := h.stack.UDPStats(); stats.DropQueueFull != 1 {
		t.Fatalf("queue insert over the lowered depth: %+v", stats)
	}
	for index := 0; index < 4; index++ {
		if _, payload := h.take(0, ids[0]); payload[0] != byte(index) {
			t.Fatalf("datagram %d is %v", index, payload)
		}
	}
	if received, _ := h.take(0, ids[0]); received.Length != 1000 {
		t.Fatalf("queued datagram over the new payload limit lost: %+v", received)
	}
	if result := h.stack.UDPSendTo(1, ids[2], peerIP, 9, nil, h.now); result != ResultOK {
		t.Fatalf("existing endpoint stopped working: %d", result)
	}
	if sent := only(t, h.drain()); sent.udp.sourcePort != ephemeral {
		t.Fatalf("existing port moved to %d", sent.udp.sourcePort)
	}
	if result := h.stack.UDPClose(0, ids[1]); result != ResultOK {
		t.Fatalf("close %d", result)
	}
	if _, result := h.stack.UDPOpen(2); result != ResultBusy {
		t.Fatal("open at the lowered total")
	}
	h.stack.UDPClose(0, ids[0])
	again, result := h.stack.UDPOpen(2)
	if result != ResultOK {
		t.Fatalf("open under the lowered total: %d", result)
	}
	if _, result := h.stack.UDPOpen(2); result != ResultBusy {
		t.Fatal("open over the lowered session share")
	}
	if port := h.bind(2, again, IPv4{}, 0); port != 2000 {
		t.Fatalf("ephemeral port %d", port)
	}
}
