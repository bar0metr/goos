package network

import (
	"encoding/binary"
	"testing"
)

// storedDefaults are the schema defaults as the configuration service holds
// them, in Field order from FieldTTL.
var storedDefaults = [TuningFields]uint64{64, 60000, 1000, 3, 20, 4000, 64000, 10000, 2, 500}

func TestParseTuningDefaults(t *testing.T) {
	values := storedDefaults
	tuning, field := ParseTuning(&values)
	if field != FieldNone || tuning != DefaultTuning() {
		t.Fatalf("parsed %+v field %d", tuning, field)
	}
	if field := DefaultTuning().Validate(); field != FieldNone {
		t.Fatalf("defaults invalid: %d", field)
	}
}

func TestParseTuningRanges(t *testing.T) {
	for index, limits := range tuningRange {
		field := FieldTTL + Field(index)
		for _, value := range []uint64{limits.min, limits.max} {
			values := storedDefaults
			values[index] = value
			// Keep the backoff ceiling legal while the start moves.
			if field == FieldDHCPInitialRetry {
				values[FieldDHCPMaxRetry-FieldTTL] = tuningRange[FieldDHCPMaxRetry-FieldTTL].max
			}
			if field == FieldDHCPMaxRetry {
				values[FieldDHCPInitialRetry-FieldTTL] = tuningRange[FieldDHCPInitialRetry-FieldTTL].min
			}
			if _, got := ParseTuning(&values); got != FieldNone {
				t.Errorf("field %d value %d refused as %d", field, value, got)
			}
		}
		bad := []uint64{limits.max + 1, ^uint64(0), 1 << 56}
		if limits.min > 0 {
			bad = append(bad, limits.min-1)
		}
		for _, value := range bad {
			values := storedDefaults
			values[index] = value
			if _, got := ParseTuning(&values); got != field {
				t.Errorf("field %d value %d: got field %d", field, value, got)
			}
		}
	}
}

func TestParseTuningCrossField(t *testing.T) {
	values := storedDefaults
	values[FieldDHCPInitialRetry-FieldTTL] = 30000
	values[FieldDHCPMaxRetry-FieldTTL] = 10000
	if _, field := ParseTuning(&values); field != FieldDHCPMaxRetry {
		t.Fatalf("field %d, want max retry", field)
	}
	values[FieldDHCPMaxRetry-FieldTTL] = 30000
	if _, field := ParseTuning(&values); field != FieldNone {
		t.Fatalf("equal retry bounds refused: %d", field)
	}
}

func TestApplyIsAtomic(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	before := h.stack.Tuning()
	bad := DefaultTuning()
	bad.TTL = 128
	bad.ProbeInterval = 0
	moved := staticConfig
	moved.Address = IPv4{10, 0, 2, 16}
	if field := h.stack.Apply(moved, bad, h.now); field != FieldProbeInterval {
		t.Fatalf("field %d", field)
	}
	if h.stack.Tuning() != before || h.stack.Snapshot(h.now).Address != hostIP {
		t.Fatal("refused tuning changed the stack")
	}
	good := DefaultTuning()
	good.TTL = 128
	broken := staticConfig
	broken.Gateway = hostIP
	if field := h.stack.Apply(broken, good, h.now); field != FieldGateway {
		t.Fatalf("field %d", field)
	}
	if h.stack.Tuning() != before {
		t.Fatal("refused configuration changed the tuning")
	}
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("refused candidate sent frames")
	}
}

func TestTuningAloneKeepsAddressAndNeighbors(t *testing.T) {
	h := readyWithPeer(t)
	tuning := DefaultTuning()
	tuning.ProbeCount, tuning.ProbeInterval = 5, 2*second
	tuning.NeighborLifetime = 10 * second
	if field := h.stack.Apply(staticConfig, tuning, h.now); field != FieldNone {
		t.Fatalf("field %d", field)
	}
	if frames := h.drain(); len(frames) != 0 {
		t.Fatalf("tuning change sent %d frames", len(frames))
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.Address != hostIP {
		t.Fatalf("snapshot %+v", snapshot)
	}
	// An entry learned before the change keeps its sixty-second expiry.
	h.now += 30 * second
	if _, ok := h.stack.lookupNeighbor(peerIP, h.now); !ok {
		t.Fatal("existing neighbor expired under the new lifetime")
	}
	// A refresh takes the new lifetime.
	h.learn(peerIP, peerMAC)
	h.now += 10 * second
	if _, ok := h.stack.lookupNeighbor(peerIP, h.now); ok {
		t.Fatal("refreshed neighbor outlived the new lifetime")
	}
}

func TestTTLAppliesToNewPackets(t *testing.T) {
	h := readyWithPeer(t)
	if request := sendEchoToPeer(h, 0, 1); request.ttl != 64 {
		t.Fatalf("default TTL %d", request.ttl)
	}
	tuning := DefaultTuning()
	tuning.TTL = 128
	h.stack.Apply(staticConfig, tuning, h.now)
	h.stack.Echo(1, peerIP, 2, second, h.now)
	h.receive(ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, icmpBytes(icmpEchoRequest, 0, 1, 1, []byte("x"))))
	frames := h.drain()
	if len(frames) != 2 {
		t.Fatalf("%d frames", len(frames))
	}
	for _, frame := range frames {
		if sent := decode(t, frame); sent.ttl != 128 {
			t.Fatalf("TTL %d after change", sent.ttl)
		}
	}
}

func TestEchoReplyRateZeroStillMatchesOwnReplies(t *testing.T) {
	h := readyWithPeer(t)
	tuning := DefaultTuning()
	tuning.EchoReplyRate = 0
	h.stack.Apply(staticConfig, tuning, h.now)
	h.receive(ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, icmpBytes(icmpEchoRequest, 0, 1, 1, []byte("x"))))
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("answered an echo with replies disabled")
	}
	request := sendEchoToPeer(h, 0, 7)
	h.receive(replyTo(request))
	if completion := h.completion(); completion.Result != ResultOK {
		t.Fatalf("own echo %+v", completion)
	}
}

func TestEchoReplyRateAboveByteRange(t *testing.T) {
	h := readyWithPeer(t)
	tuning := DefaultTuning()
	tuning.EchoReplyRate = 300
	h.stack.Apply(staticConfig, tuning, h.now)
	request := ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, icmpBytes(icmpEchoRequest, 0, 1, 1, []byte("x")))
	answered := 0
	for index := 0; index < 400; index++ {
		h.receive(request)
		answered += len(h.drain())
		h.now += millisecond
	}
	if answered != 300 {
		t.Fatalf("answered %d, want 300", answered)
	}
}

func TestResolutionKeepsItsStartingTuning(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	h.stack.Echo(0, peerIP, 1, MaxEchoTimeout, h.now)
	h.drain()
	tuning := DefaultTuning()
	tuning.ResolveAttempts, tuning.ResolveInterval = 1, 100*millisecond
	h.stack.Apply(staticConfig, tuning, h.now)
	// The running resolution still sends three requests a second apart.
	for attempt := 2; attempt <= 3; attempt++ {
		h.advance(second)
		if request := only(t, h.drain()); request.arp.targetIP != peerIP {
			t.Fatalf("attempt %d: %+v", attempt, request.arp)
		}
	}
	h.advance(second)
	if completion := h.completion(); completion.Result != ResultNeighborUnreachable {
		t.Fatalf("completion %+v", completion)
	}
	// A new one uses the new values: one request, failure 100 ms later.
	h.stack.Echo(0, peerIP, 2, MaxEchoTimeout, h.now)
	only(t, h.drain())
	h.advance(100 * millisecond)
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("retried with one attempt configured")
	}
	if completion := h.completion(); completion.Result != ResultNeighborUnreachable {
		t.Fatalf("completion %+v", completion)
	}
}

func TestProbeTuning(t *testing.T) {
	h := newHarness(t)
	tuning := DefaultTuning()
	tuning.ProbeCount, tuning.ProbeInterval = 3, 200*millisecond
	h.stack.Apply(staticConfig, tuning, h.now)
	h.stack.LinkUp(hostMAC, MTU, h.now)
	for probe := 1; probe <= 3; probe++ {
		if sent := only(t, h.drain()); sent.arp.senderIP != zeroIPv4 {
			t.Fatalf("probe %d: %+v", probe, sent.arp)
		}
		h.advance(200*millisecond - 1)
		if state := h.stack.Snapshot(h.now).State; state != StateConfiguring {
			t.Fatalf("probe %d: state %d", probe, state)
		}
		h.advance(1)
	}
	if announce := only(t, h.drain()); announce.arp.senderIP != hostIP {
		t.Fatalf("announcement %+v", announce.arp)
	}
	if state := h.stack.Snapshot(h.now).State; state != StateReady {
		t.Fatalf("state %d", state)
	}
}

func TestDHCPRetryTuning(t *testing.T) {
	h := newHarness(t)
	tuning := DefaultTuning()
	tuning.DHCPInitialRetry, tuning.DHCPMaxRetry = second, 4*second
	h.stack.Apply(Config{Mode: ModeDHCP}, tuning, h.now)
	h.stack.LinkUp(hostMAC, MTU, h.now)
	discover := onlyClient(t, h.drain())
	last := h.now
	for _, base := range []uint64{1, 2, 4, 4} {
		deadline, ok := h.stack.Deadline()
		if !ok {
			t.Fatal("no retransmission scheduled")
		}
		// Below two seconds the jitter shrinks to half the base.
		spread := min(second, base*second/2)
		if wait := deadline - last; wait < base*second-spread || wait > base*second+spread || wait == 0 {
			t.Fatalf("base %d s: wait %d ns", base, wait)
		}
		h.runUntil(deadline)
		if again := onlyClient(t, h.drain()); again.kind != dhcpDiscover || again.xid != discover.xid {
			t.Fatalf("retransmission %+v", again)
		}
		last = h.now
	}
}

func TestDHCPConflictRetryTuning(t *testing.T) {
	h, discover := dhcpHarness(t)
	tuning := DefaultTuning()
	tuning.DHCPConflictRetry = 2 * second
	h.stack.Apply(Config{Mode: ModeDHCP}, tuning, h.now)
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("tuning change restarted discovery")
	}
	offerAndAck(h, discover, leaseOptions(dhcpAck))
	h.drain()
	h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: hostIP, targetMAC: hostMAC}))
	h.drain()
	h.runUntil(h.now + 2*second - 1)
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("restarted early")
	}
	h.advance(1)
	if restart := onlyClient(t, h.drain()); restart.kind != dhcpDiscover {
		t.Fatalf("restart %+v", restart)
	}
}

func TestDHCPTuningKeepsLease(t *testing.T) {
	h, _ := boundHarness(t)
	before := h.stack.dhcp
	tuning := DefaultTuning()
	tuning.DHCPInitialRetry, tuning.DHCPMaxRetry = 60*second, 300*second
	h.stack.Apply(Config{Mode: ModeDHCP}, tuning, h.now)
	if h.stack.dhcp != before || h.stack.Snapshot(h.now).Address != hostIP {
		t.Fatal("tuning change disturbed the lease")
	}
}

func TestDHCPMaxMessageFollowsMTU(t *testing.T) {
	for _, mtu := range []uint16{minMTU, 1000, MTU} {
		h := newHarness(t)
		h.stack.LinkUp(hostMAC, mtu, h.now)
		discover := onlyClient(t, h.drain())
		value := discover.options[optionMaxMessage]
		if len(value) != 2 || binary.BigEndian.Uint16(value) != mtu {
			t.Fatalf("MTU %d: option 57 %v", mtu, value)
		}
		if snapshot := h.stack.Snapshot(h.now); snapshot.MTU != mtu {
			t.Fatalf("snapshot MTU %d, want %d", snapshot.MTU, mtu)
		}
	}
}
