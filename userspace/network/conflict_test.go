package network

import "testing"

// stamped is an outgoing frame and when it left.
type stamped struct {
	at    uint64
	frame sentFrame
}

// claim steps from deadline to deadline until the stack is ready or failed,
// and returns every frame it sent on the way, the first announcement last.
func (h *harness) claim() []stamped {
	h.t.Helper()
	var sent []stamped
	for step := 0; step < 100; step++ {
		for _, frame := range h.drain() {
			sent = append(sent, stamped{h.now, decode(h.t, frame)})
		}
		if state := h.stack.Snapshot(h.now).State; state == StateReady || state == StateFailed {
			return sent
		}
		deadline, ok := h.stack.Deadline()
		if !ok {
			h.t.Fatal("claim stalled without a deadline")
		}
		h.now = deadline
		h.stack.Advance(h.now)
		h.checkDeadline()
	}
	h.t.Fatal("claim did not finish")
	return nil
}

// checkClaim verifies the RFC 5227 sequence: a random wait of at most
// ProbeWait, ProbeCount probes ProbeMin..ProbeMax apart, announceWait of
// quiet, then the first announcement.
func checkClaim(t *testing.T, sent []stamped, candidate IPv4, start uint64, tuning Tuning) {
	t.Helper()
	if len(sent) != int(tuning.ProbeCount)+1 {
		t.Fatalf("%d frames, want %d probes and an announcement", len(sent), tuning.ProbeCount)
	}
	for index, entry := range sent[:tuning.ProbeCount] {
		arp := entry.frame.arp
		if arp.operation != arpRequest || arp.senderIP != zeroIPv4 || arp.targetIP != candidate ||
			arp.senderMAC != hostMAC || entry.frame.ether.destination != broadcastMAC {
			t.Fatalf("probe %d: %+v", index, arp)
		}
		if index == 0 {
			if entry.at-start > tuning.ProbeWait {
				t.Fatalf("first probe after %d ns", entry.at-start)
			}
			continue
		}
		if gap := entry.at - sent[index-1].at; gap < tuning.ProbeMin || gap > tuning.ProbeMax {
			t.Fatalf("probe %d gap %d ns", index, gap)
		}
	}
	last := sent[len(sent)-1]
	if last.frame.arp.senderIP != candidate || last.frame.arp.targetIP != candidate ||
		last.frame.ether.destination != broadcastMAC {
		t.Fatalf("announcement %+v", last.frame.arp)
	}
	if quiet := last.at - sent[len(sent)-2].at; quiet != announceWait {
		t.Fatalf("quiet period %d ns", quiet)
	}
}

// finishAnnouncements runs the remaining announcements of a live address.
func (h *harness) finishAnnouncements() {
	h.t.Helper()
	for sent := 1; sent < int(h.stack.tuning.AnnounceCount); sent++ {
		h.advance(h.stack.tuning.AnnounceInterval - 1)
		if frames := h.drain(); len(frames) != 0 {
			h.t.Fatal("announcement sent early")
		}
		h.advance(1)
		announce := only(h.t, h.drain())
		if announce.arp.senderIP != h.stack.address || announce.arp.targetIP != h.stack.address {
			h.t.Fatalf("announcement %d: %+v", sent+1, announce.arp)
		}
	}
}

func TestProbeRandomnessStaysInBounds(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		h := newHarness(t)
		h.stack.Init(seed * 0x9e3779b97f4a7c15)
		h.stack.Configure(staticConfig, h.now)
		h.stack.LinkUp(hostMAC, MTU, h.now)
		checkClaim(t, h.claim(), hostIP, h.start, defaults)
	}
}

func TestProbeWaitsDiffer(t *testing.T) {
	seen := map[uint64]bool{}
	for seed := uint64(1); seed <= 8; seed++ {
		h := newHarness(t)
		h.stack.Init(seed)
		h.stack.Configure(staticConfig, h.now)
		h.stack.LinkUp(hostMAC, MTU, h.now)
		deadline, _ := h.stack.Deadline()
		seen[deadline] = true
	}
	if len(seen) < 6 {
		t.Fatalf("initial waits repeat: %d distinct of 8", len(seen))
	}
}

func TestZeroProbeWaitProbesAtOnce(t *testing.T) {
	h := newHarness(t)
	tuning := DefaultTuning()
	tuning.ProbeWait = 0
	h.stack.Apply(staticConfig, tuning, h.now)
	h.stack.LinkUp(hostMAC, MTU, h.now)
	if probe := only(t, h.drain()); probe.arp.senderIP != zeroIPv4 || probe.arp.targetIP != hostIP {
		t.Fatalf("probe %+v", probe.arp)
	}
}

func TestTwoAnnouncements(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	if counters := h.stack.Snapshot(h.now).Conflicts; counters.Probes != 3 || counters.Announcements != 2 {
		t.Fatalf("counters %+v", counters)
	}
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("deadline after the announcements")
	}
}

func claimFrom(mac MAC, address IPv4) []byte {
	return arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: mac, senderIP: address, targetIP: address})
}

func TestActiveConflictDefendedThenRelinquished(t *testing.T) {
	h := readyWithPeer(t)
	h.receive(claimFrom(otherMAC, hostIP))
	defence := only(t, h.drain())
	if defence.arp.senderIP != hostIP || defence.arp.targetIP != hostIP || defence.arp.senderMAC != hostMAC {
		t.Fatalf("defence %+v", defence.arp)
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.Conflicts.Defenses != 1 ||
		snapshot.Conflicts.ActiveConflicts != 1 {
		t.Fatalf("after first conflict %+v", snapshot)
	}
	h.advance(defaults.DefendInterval - 1)
	// A pending echo must fail once the address is given up.
	h.stack.Echo(0, peerIP, 1, second, h.now)
	h.drain()
	h.receive(claimFrom(otherMAC, hostIP))
	if frames := h.drain(); len(frames) != 0 {
		t.Fatalf("static relinquish sent %d frames", len(frames))
	}
	snapshot := h.stack.Snapshot(h.now)
	if snapshot.State != StateFailed || snapshot.Address != zeroIPv4 || snapshot.Conflicts.Relinquished != 1 {
		t.Fatalf("after second conflict %+v", snapshot)
	}
	if completion := h.completion(); completion.Result != ResultReconfigured {
		t.Fatalf("echo %+v", completion)
	}
	if result := h.stack.Echo(1, peerIP, 2, second, h.now); result != ResultNoAddress {
		t.Fatalf("echo after relinquish %d", result)
	}
}

func TestConflictOutsideDefendIntervalDefendedAgain(t *testing.T) {
	h := readyWithPeer(t)
	h.receive(claimFrom(otherMAC, hostIP))
	h.drain()
	h.advance(defaults.DefendInterval)
	h.receive(claimFrom(otherMAC, hostIP))
	only(t, h.drain())
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.Conflicts.Defenses != 2 {
		t.Fatalf("snapshot %+v", snapshot)
	}
}

func TestDHCPRelinquishDeclinesAndRediscovers(t *testing.T) {
	h, _ := boundHarness(t)
	h.receive(claimFrom(otherMAC, hostIP))
	h.drain()
	h.advance(second)
	h.receive(claimFrom(otherMAC, hostIP))
	decline := onlyClient(t, h.drain())
	if decline.kind != dhcpDecline || string(decline.options[optionRequestedIP]) != string(hostIP[:]) {
		t.Fatalf("decline %+v", decline)
	}
	snapshot := h.stack.Snapshot(h.now)
	if snapshot.State != StateConfiguring || snapshot.Address != zeroIPv4 || snapshot.Conflicts.Relinquished != 1 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	if _, count, _ := h.stack.DNSServers(); count != 0 {
		t.Fatal("DNS servers outlived the lease")
	}
	h.runUntil(h.now + defaults.DHCPConflictRetry)
	if restart := onlyClient(t, h.drain()); restart.kind != dhcpDiscover {
		t.Fatalf("restart %+v", restart)
	}
}

func TestProbeAnsweredForLiveAddress(t *testing.T) {
	h := readyWithPeer(t)
	h.receive(arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: otherMAC, targetIP: hostIP}))
	reply := only(t, h.drain())
	if reply.arp.operation != arpReply || reply.arp.senderIP != hostIP || reply.arp.targetMAC != otherMAC ||
		reply.ether.destination != otherMAC {
		t.Fatalf("reply %+v", reply.arp)
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.Conflicts.ActiveConflicts != 0 {
		t.Fatalf("a probe counted as a conflict: %+v", snapshot)
	}
}

func TestRepeatedConflictsRateLimitDHCP(t *testing.T) {
	h, discover := dhcpHarness(t)
	tuning := DefaultTuning()
	tuning.MaxConflicts = 3
	h.stack.Apply(Config{Mode: ModeDHCP}, tuning, h.now)
	for round := 1; round <= 3; round++ {
		offerAndAck(h, discover, leaseOptions(dhcpAck))
		h.drain()
		h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: hostIP, targetMAC: hostMAC}))
		h.drain()
		wait := tuning.DHCPConflictRetry
		if round == 3 {
			wait = tuning.ConflictRateLimit
			if !h.stack.Snapshot(h.now).RateLimited {
				t.Fatal("not rate limited after the third conflict")
			}
		}
		h.runUntil(h.now + wait - 1)
		if frames := h.drain(); len(frames) != 0 {
			t.Fatalf("round %d: restarted early", round)
		}
		h.advance(1)
		discover = onlyClient(t, h.drain())
	}
	snapshot := h.stack.Snapshot(h.now)
	if snapshot.Conflicts.RateLimits != 1 || snapshot.Conflicts.ProbeConflicts != 3 || snapshot.RateLimited ||
		snapshot.ConflictRun != 3 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	// An address that holds through its announcements ends the run.
	offerAndAck(h, discover, leaseOptions(dhcpAck))
	h.runUntil(h.now + claimTime + tuning.AnnounceInterval)
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.ConflictRun != 0 {
		t.Fatalf("after a held address %+v", snapshot)
	}
}

func TestRepeatedConflictsRateLimitStatic(t *testing.T) {
	h := newHarness(t)
	tuning := DefaultTuning()
	tuning.MaxConflicts = 1
	h.stack.Apply(staticConfig, tuning, h.now)
	h.stack.LinkUp(hostMAC, MTU, h.now)
	h.runUntil(h.now + tuning.ProbeWait)
	h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: hostIP, targetMAC: hostMAC}))
	if state := h.stack.Snapshot(h.now).State; state != StateFailed {
		t.Fatalf("state %d", state)
	}
	h.drain()
	moved := staticConfig
	moved.Address = IPv4{10, 0, 2, 16}
	h.stack.Apply(moved, tuning, h.now)
	deadline, ok := h.stack.Deadline()
	if !ok || deadline-h.now != tuning.ConflictRateLimit || !h.stack.Snapshot(h.now).RateLimited {
		t.Fatalf("next claim not rate limited: %d", deadline-h.now)
	}
}

func TestTuningReloadKeepsRunningProbe(t *testing.T) {
	h := newHarness(t)
	h.stack.Configure(staticConfig, h.now)
	h.stack.LinkUp(hostMAC, MTU, h.now)
	changed := DefaultTuning()
	changed.ProbeCount, changed.ProbeMin, changed.ProbeMax = 1, 100*millisecond, 100*millisecond
	h.stack.Apply(staticConfig, changed, h.now)
	checkClaim(t, h.claim(), hostIP, h.start, defaults)
}

func TestLoopbackNeverProbes(t *testing.T) {
	invalid := staticConfig
	invalid.Address = IPv4{127, 0, 0, 1}
	invalid.Prefix = 8
	invalid.Gateway = zeroIPv4
	h := newHarness(t)
	if field := h.stack.Configure(invalid, h.now); field != FieldAddress {
		t.Fatalf("loopback static address accepted: %d", field)
	}
	h.readyStatic(staticConfig)
	h.receive(claimFrom(otherMAC, IPv4{127, 0, 0, 1}))
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("answered ARP for a loopback address")
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.Conflicts.ActiveConflicts != 0 || snapshot.State != StateReady {
		t.Fatalf("snapshot %+v", snapshot)
	}
}
