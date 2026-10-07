package network

import "testing"

// dnsIP is the lease's DNS server; remoteDNS sits behind the gateway.
var (
	dnsIP     = IPv4{10, 0, 2, 3}
	remoteDNS = IPv4{198, 51, 100, 53}
)

func udpFrame(source IPv4, destination IPv4, sourcePort, destinationPort uint16, payload []byte) []byte {
	return ipv4Frame(hostMAC, gatewayMAC, source, destination, protocolUDP,
		udpBytes(source, destination, sourcePort, destinationPort, payload))
}

// sendQuery opens a datagram to the DNS server through the gateway and
// returns the frame that left.
func sendQuery(h *harness, slot int, payload []byte) sentFrame {
	h.t.Helper()
	if result := h.stack.SendDatagram(slot, remoteDNS, 53, payload, 2*second, h.now); result != ResultOK {
		h.t.Fatalf("send %d", result)
	}
	sent := only(h.t, h.drain())
	if sent.ip.protocol != protocolUDP || sent.ip.source != hostIP || sent.ip.destination != remoteDNS ||
		sent.ether.destination != gatewayMAC || sent.udp.destinationPort != 53 || string(sent.udp.payload) != string(payload) {
		h.t.Fatalf("query %+v", sent)
	}
	if sent.udp.sourcePort < ephemeralFirst {
		h.t.Fatalf("source port %d", sent.udp.sourcePort)
	}
	return sent
}

func TestDatagramRoundTrip(t *testing.T) {
	h := readyWithPeer(t)
	sent := sendQuery(h, 2, []byte("query"))
	h.receive(udpFrame(remoteDNS, hostIP, 53, sent.udp.sourcePort, []byte("answer")))
	buffer := make([]byte, DatagramPayload)
	done, ok := h.stack.DatagramCompletion(buffer)
	if !ok || done.Slot != 2 || done.Result != ResultOK || done.Source != remoteDNS || done.Port != 53 ||
		string(buffer[:done.Length]) != "answer" {
		t.Fatalf("completion %+v", done)
	}
	// The port closed with the first reply.
	before := h.stack.Snapshot(h.now).Drop
	h.receive(udpFrame(remoteDNS, hostIP, 53, sent.udp.sourcePort, []byte("again")))
	if h.stack.Snapshot(h.now).Drop != before+1 {
		t.Fatal("second reply accepted")
	}
}

func TestDatagramRepliesMustMatch(t *testing.T) {
	h := readyWithPeer(t)
	sent := sendQuery(h, 0, []byte("query"))
	port := sent.udp.sourcePort
	for name, frame := range map[string][]byte{
		"other server":  udpFrame(IPv4{10, 0, 2, 4}, hostIP, 53, port, []byte("x")),
		"other port":    udpFrame(remoteDNS, hostIP, 5353, port, []byte("x")),
		"other dest":    udpFrame(remoteDNS, hostIP, 53, port+1, []byte("x")),
		"loopback from": udpFrame(IPv4{127, 0, 0, 1}, hostIP, 53, port, []byte("x")),
	} {
		h.receive(frame)
		if _, ok := h.stack.DatagramCompletion(make([]byte, DatagramPayload)); ok {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestDatagramTimeoutAndLateReply(t *testing.T) {
	h := readyWithPeer(t)
	sent := sendQuery(h, 0, []byte("query"))
	h.advance(2 * second)
	done, ok := h.stack.DatagramCompletion(make([]byte, DatagramPayload))
	if !ok || done.Result != ResultTimeout {
		t.Fatalf("completion %+v", done)
	}
	before := h.stack.Snapshot(h.now).Drop
	h.receive(udpFrame(remoteDNS, hostIP, 53, sent.udp.sourcePort, []byte("late")))
	if h.stack.Snapshot(h.now).Drop != before+1 {
		t.Fatal("late reply accepted")
	}
}

func TestDatagramPortsRandomAndDistinct(t *testing.T) {
	h := readyWithPeer(t)
	seen := map[uint16]bool{}
	for slot := 0; slot < MaxDatagrams; slot++ {
		port := sendQuery(h, slot, []byte{byte(slot)}).udp.sourcePort
		if seen[port] {
			t.Fatalf("port %d reused", port)
		}
		seen[port] = true
	}
	if result := h.stack.SendDatagram(0, remoteDNS, 53, []byte("x"), second, h.now); result != ResultBusy {
		t.Fatalf("busy slot %d", result)
	}
	if result := h.stack.SendDatagram(MaxDatagrams, remoteDNS, 53, []byte("x"), second, h.now); result != ResultInvalidArgument {
		t.Fatalf("bad slot %d", result)
	}
}

func TestDatagramWaitsForARPAndFailsWithLink(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	if result := h.stack.SendDatagram(0, peerIP, 53, []byte("q"), 5*second, h.now); result != ResultOK {
		t.Fatalf("send %d", result)
	}
	if request := only(t, h.drain()); request.arp.targetIP != peerIP {
		t.Fatalf("expected ARP, got %+v", request)
	}
	h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: peerIP, targetMAC: hostMAC, targetIP: hostIP}))
	if sent := only(t, h.drain()); sent.ip.destination != peerIP || sent.udp.destinationPort != 53 {
		t.Fatalf("datagram %+v", sent)
	}
	h.stack.LinkDown(ResultDown, h.now)
	done, ok := h.stack.DatagramCompletion(make([]byte, DatagramPayload))
	if !ok || done.Result != ResultDown {
		t.Fatalf("completion %+v", done)
	}
	if result := h.stack.SendDatagram(0, peerIP, 53, []byte("q"), second, h.now); result != ResultDown {
		t.Fatalf("send while down %d", result)
	}
}

func TestDHCPDNSServers(t *testing.T) {
	h, _ := boundHarness(t)
	servers, count, generation := h.stack.DNSServers()
	if count != 1 || servers[0] != dnsIP || generation == 0 {
		t.Fatalf("servers %v %d %d", servers, count, generation)
	}
	// A tuning reload keeps them.
	h.stack.Apply(Config{Mode: ModeDHCP}, DefaultTuning(), h.now)
	if _, again, same := h.stack.DNSServers(); again != 1 || same != generation {
		t.Fatal("reload changed the DNS servers")
	}
	h.stack.LinkDown(ResultDown, h.now)
	if _, none, later := h.stack.DNSServers(); none != 0 || later == generation {
		t.Fatal("DNS servers outlived the link")
	}
}

func TestDHCPOption6Parsing(t *testing.T) {
	parse := func(value []byte) (dhcpMessage, bool) {
		var message dhcpMessage
		return message, message.option(optionDNS, value, true) && true
	}
	if _, ok := parse([]byte{1, 2, 3}); ok {
		t.Fatal("3-byte option 6 accepted")
	}
	if _, ok := parse([]byte{1, 2, 3, 4, 5}); ok {
		t.Fatal("5-byte option 6 accepted")
	}
	var message dhcpMessage
	value := []byte{0, 0, 0, 0, 127, 0, 0, 1, 255, 255, 255, 255, 224, 0, 0, 1,
		10, 0, 0, 1, 10, 0, 0, 2, 10, 0, 0, 3, 10, 0, 0, 4, 10, 0, 0, 5}
	if !message.option(optionDNS, value, true) || message.dnsCount != MaxDNSServers ||
		message.dns[0] != (IPv4{10, 0, 0, 1}) || message.dns[3] != (IPv4{10, 0, 0, 4}) {
		t.Fatalf("parsed %v %d", message.dns, message.dnsCount)
	}
	if message.option(optionDNS, value[:4], true) {
		t.Fatal("repeated option 6 accepted")
	}
}

func TestDHCPRequestsDNSOption(t *testing.T) {
	_, discover := dhcpHarness(t)
	found := false
	for _, code := range discover.options[optionParameters] {
		found = found || code == optionDNS
	}
	if !found {
		t.Fatal("option 6 not requested")
	}
}
