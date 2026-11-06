//go:build !goos_userspace

package main

import (
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/netapi"
	"goos/userspace/network"
)

// The capacity block carries the stack's counters and budgets and the
// session counts unchanged, saturates what its fields cannot hold, and
// survives the GNETAPI1 round trip.
func TestTCPCapacityReflectsStackStats(t *testing.T) {
	var stack network.Stack
	stack.Init(1)
	stack.SetTCPMemoryBudget(1<<20, 1<<16)
	stack.SetTCPTimeWaitBudget(1<<17, 300)
	stats := stack.TCPStats()
	stats.MemoryBytes, stats.TimeWait, stats.TimeWaitHeld = 4096, 12, 3
	stats.DropMemory, stats.DropBacklog, stats.MemoryCapped = 5, 6, 7
	stats.LookupProbes, stats.MaxSegmentProbes = 1<<40, 1<<33
	got := tcpCapacity(&stats, stack.TCPBudgets(), sessionUse{command: 2, udp: 1, tcp: 46})
	want := netapi.TCPCapacity{MemoryBytes: 4096, MemoryBudget: 1 << 20, OwnerMemoryBudget: 1 << 16,
		TimeWait: 12, TimeWaitBudget: 1<<16 - 1, OwnerTimeWaitBudget: 300, TimeWaitHeld: 3,
		DropMemory: 5, DropBacklog: 6, MemoryCapped: 7, LookupProbes: 1 << 40, MaxSegmentProbes: 1<<32 - 1,
		CommandSessions: 2, CommandSessionLimit: network.MaxSessions, UDPSessions: 1, UDPSessionLimit: network.MaxUDPSessions,
		TCPSessions: 46, TCPSessionLimit: tcpSessionSlots}
	if got != want {
		t.Fatalf("capacity %+v, want %+v", got, want)
	}
	if tcpSessionSlots != 46 {
		t.Fatalf("TCP session slots %d", tcpSessionSlots)
	}
	reply := netapi.Reply{Opcode: netapi.OpTCPStatus, TCPListenBacklog: 128, Capacity: got}
	var wire ipc.Message
	var decoded netapi.Reply
	if !netapi.EncodeReply(&reply, 1, &wire) || !netapi.DecodeReply(&wire, 1, netapi.OpTCPStatus, &decoded) || decoded != reply {
		t.Fatalf("round trip %+v", decoded.Capacity)
	}
}
