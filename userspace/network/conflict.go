package network

// IPv4 address conflict detection (RFC 5227). Every address, from a DHCP
// lease or from static configuration, is probed before it goes live,
// announced once it does, and defended while it stays.
//
// Probing waits a random time up to ProbeWait, sends ProbeCount ARP probes a
// random ProbeMin..ProbeMax apart, and waits announceWait after the last.
// Any ARP from another host that uses or probes for the candidate rejects
// it. A live address is announced AnnounceCount times AnnounceInterval
// apart. When another host claims the live address, the stack defends it
// with one announcement; a second claim within DefendInterval makes it give
// the address up: DHCP declines the lease and starts again, static fails
// until the next reload or link-up. MaxConflicts conflicts in a row, with no
// address held through its announcements in between, delay the next claim
// by ConflictRateLimit.

// Conflicts counts conflict-detection events since the stack started.
type Conflicts struct {
	Probes          uint64
	ProbeConflicts  uint64
	Announcements   uint64
	ActiveConflicts uint64
	Defenses        uint64
	Relinquished    uint64
	RateLimits      uint64
}

// probe checks one candidate before it goes live. count, min and max are the
// tuning the probe started with.
type probe struct {
	active    bool
	candidate IPv4
	sent      uint8
	deadline  uint64
	count     uint8
	min, max  uint64
}

// announcer repeats the announcement of a newly live address with the tuning
// it started with.
type announcer struct {
	active   bool
	sent     uint8
	count    uint8
	interval uint64
	deadline uint64
}

// between returns a random time in low..high.
func (stack *Stack) between(low, high uint64) uint64 {
	return low + stack.next()%(high-low+1)
}

// startProbe begins checking candidate. A static claim after MaxConflicts
// conflicts waits ConflictRateLimit first; a DHCP one has already waited in
// the decline backoff.
func (stack *Stack) startProbe(candidate IPv4, now uint64) {
	tuning := &stack.tuning
	stack.probe = probe{active: true, candidate: candidate, count: tuning.ProbeCount,
		min: tuning.ProbeMin, max: tuning.ProbeMax}
	wait := stack.between(0, tuning.ProbeWait)
	if stack.config.Mode == ModeStatic && stack.conflicts >= tuning.MaxConflicts {
		wait = tuning.ConflictRateLimit
		stack.limitRate()
	}
	if wait == 0 {
		stack.sendProbe(now)
		return
	}
	stack.probe.deadline = addTime(now, wait)
}

// sendProbe asks who owns the candidate without claiming it: a zero sender
// address keeps peers from caching our MAC for an address we do not hold yet.
func (stack *Stack) sendProbe(now uint64) {
	stack.rateLimited = false
	stack.sendARP(broadcastMAC, arpPacket{
		operation: arpRequest,
		senderMAC: stack.mac,
		targetIP:  stack.probe.candidate,
	})
	increment(&stack.counters.Probes)
	stack.probe.sent++
	wait := uint64(announceWait)
	if stack.probe.sent < stack.probe.count {
		wait = stack.between(stack.probe.min, stack.probe.max)
	}
	stack.probe.deadline = addTime(now, wait)
}

func (stack *Stack) advanceProbe(now uint64) {
	if stack.probe.sent < stack.probe.count {
		stack.sendProbe(now)
		return
	}
	candidate := stack.probe.candidate
	stack.probe = probe{}
	if stack.config.Mode == ModeDHCP {
		stack.dhcpProbePassed(candidate, now)
		return
	}
	stack.applyAddress(candidate, stack.config.Prefix, stack.config.Gateway, now)
}

// conflicts reports a packet that shows another host holding the candidate
// address, or probing for it at the same time as we are.
func (candidate *probe) conflicts(packet *arpPacket) bool {
	if packet.senderIP == candidate.candidate {
		return true
	}
	return packet.operation == arpRequest && packet.senderIP == zeroIPv4 && packet.targetIP == candidate.candidate
}

// probeConflict reacts to another host using, or probing for, our candidate.
// A static conflict needs an operator to pick another address, so the stack
// stays Failed until a reload.
func (stack *Stack) probeConflict(now uint64) {
	increment(&stack.counters.ProbeConflicts)
	stack.countConflict()
	candidate := stack.probe.candidate
	stack.probe = probe{}
	if stack.config.Mode == ModeDHCP {
		stack.dhcpDecline(candidate, now)
		return
	}
	stack.state = StateFailed
	stack.failResult = ResultNoAddress
}

// applyAddress makes a probed address live and starts its announcements, so
// peers replace any stale mapping for it.
func (stack *Stack) applyAddress(address IPv4, prefix uint8, gateway IPv4, now uint64) {
	stack.address, stack.prefix, stack.gateway = address, prefix, gateway
	stack.state = StateReady
	stack.defended = false
	stack.announcer = announcer{active: true, count: stack.tuning.AnnounceCount, interval: stack.tuning.AnnounceInterval}
	stack.advanceAnnouncer(now)
}

// advanceAnnouncer sends the next announcement. An address that has been
// announced the full count without a conflict has held, which ends any run
// of conflicts.
func (stack *Stack) advanceAnnouncer(now uint64) {
	stack.announce()
	stack.announcer.sent++
	if stack.announcer.sent < stack.announcer.count {
		stack.announcer.deadline = addTime(now, stack.announcer.interval)
		return
	}
	stack.announcer = announcer{}
	stack.conflicts = 0
}

// announce broadcasts that we hold our address: an ARP request with our
// address as both sender and target.
func (stack *Stack) announce() {
	increment(&stack.counters.Announcements)
	stack.sendARP(broadcastMAC, arpPacket{
		operation: arpRequest,
		senderMAC: stack.mac,
		senderIP:  stack.address,
		targetIP:  stack.address,
	})
}

// activeConflict handles another host claiming our live address. The first
// claim, or one more than DefendInterval after the last defence, is answered
// with an announcement; a second claim inside the interval gives the address
// up.
func (stack *Stack) activeConflict(now uint64) {
	increment(&stack.counters.ActiveConflicts)
	if !stack.defended || now-stack.defendedAt >= stack.tuning.DefendInterval {
		stack.defended, stack.defendedAt = true, now
		increment(&stack.counters.Defenses)
		stack.announce()
		return
	}
	increment(&stack.counters.Relinquished)
	stack.countConflict()
	address := stack.address
	// Nothing queued may leave from an address we no longer hold.
	stack.flushFrames()
	stack.resetAddressing(ResultReconfigured)
	if stack.config.Mode == ModeDHCP {
		stack.dhcpDecline(address, now)
		return
	}
	stack.state = StateFailed
	stack.failResult = ResultNoAddress
}

func (stack *Stack) countConflict() {
	if stack.conflicts != 0xff {
		stack.conflicts++
	}
}

// conflictWait is how long the DHCP client waits after declining an
// address: the conflict delay, or ConflictRateLimit once MaxConflicts
// conflicts have happened in a row.
func (stack *Stack) conflictWait() uint64 {
	wait := stack.tuning.DHCPConflictRetry
	if stack.conflicts >= stack.tuning.MaxConflicts {
		wait = max(wait, stack.tuning.ConflictRateLimit)
		stack.limitRate()
	}
	return wait
}

func (stack *Stack) limitRate() {
	stack.rateLimited = true
	increment(&stack.counters.RateLimits)
}

// answerProbe replies to another host probing for our live address, so it
// sees the address is taken.
func (stack *Stack) answerProbe(packet *arpPacket) {
	stack.sendARP(packet.senderMAC, arpPacket{
		operation: arpReply,
		senderMAC: stack.mac,
		senderIP:  stack.address,
		targetMAC: packet.senderMAC,
	})
}
