package network

import "testing"

func TestLoopbackEchoWithoutDevice(t *testing.T) {
	h := newHarness(t)
	for index, destination := range []IPv4{{127, 0, 0, 1}, {127, 1, 2, 3}, {127, 255, 255, 254}} {
		if result := h.stack.Echo(index, destination, uint16(index), second, h.now); result != ResultOK {
			t.Fatalf("%v: %d", destination, result)
		}
		completion := h.completion()
		if completion.Result != ResultOK || completion.Responder != destination || completion.RTT != 0 ||
			completion.Sequence != uint16(index) {
			t.Fatalf("%v: %+v", destination, completion)
		}
	}
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("loopback echo reached the wire")
	}
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("loopback echo left a timer")
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.Loopback != 3 || snapshot.TX != 0 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	for _, special := range []IPv4{{127, 0, 0, 0}, {127, 255, 255, 255}} {
		if result := h.stack.Echo(0, special, 1, second, h.now); result != ResultInvalidArgument {
			t.Fatalf("%v: %d", special, result)
		}
	}
}

func TestLoopbackNeverUsesTheLink(t *testing.T) {
	h := readyWithPeer(t)
	resolutions := h.stack.resolutions
	if result := h.stack.Echo(0, IPv4{127, 0, 0, 1}, 1, second, h.now); result != ResultOK {
		t.Fatalf("echo %d", result)
	}
	h.completion()
	if frames := h.drain(); len(frames) != 0 || h.stack.resolutions != resolutions {
		t.Fatal("loopback echo used ARP or the link")
	}
}

func TestLoopbackSurvivesLinkChanges(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.LinkDown(ResultDown, h.now)
	if result := h.stack.Echo(0, IPv4{127, 0, 0, 1}, 1, second, h.now); result != ResultOK {
		t.Fatalf("echo while down %d", result)
	}
	h.completion()
	h.stack.Fail(h.now)
	if result := h.stack.Echo(0, IPv4{127, 0, 0, 1}, 2, second, h.now); result != ResultOK {
		t.Fatalf("echo while failed %d", result)
	}
	h.completion()
	h.stack.Apply(staticConfig, DefaultTuning(), h.now)
	if result := h.stack.Echo(0, IPv4{127, 0, 0, 1}, 3, second, h.now); result != ResultOK {
		t.Fatalf("echo after reload %d", result)
	}
}

func TestLoopbackFromTheLinkDropped(t *testing.T) {
	h := readyWithPeer(t)
	body := icmpBytes(icmpEchoRequest, 0, 1, 1, []byte("x"))
	for _, frame := range [][]byte{
		ipv4Frame(hostMAC, peerMAC, IPv4{127, 0, 0, 1}, hostIP, protocolICMP, body),
		ipv4Frame(hostMAC, peerMAC, peerIP, IPv4{127, 0, 0, 1}, protocolICMP, body),
		arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: IPv4{127, 0, 0, 1}, targetIP: hostIP}),
	} {
		before := h.stack.Snapshot(h.now).Drop
		h.receive(frame)
		if frames := h.drain(); len(frames) != 0 || h.stack.Snapshot(h.now).Drop != before+1 {
			t.Fatal("a loopback address from the link was accepted")
		}
	}
}
