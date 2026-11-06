//go:build !goos_userspace

package main

import (
	"testing"

	"goos/userspace/netapi"
)

func TestCapacityLines(t *testing.T) {
	c := netapi.TCPCapacity{MemoryBytes: 4096, MemoryBudget: 41943040, OwnerMemoryBudget: 8388608, DropMemory: 2, MemoryCapped: 3,
		TimeWait: 12, TimeWaitBudget: 8192, OwnerTimeWaitBudget: 2048, TimeWaitHeld: 1,
		LookupProbes: 900, MaxSegmentProbes: 4,
		CommandSessions: 1, CommandSessionLimit: 8, UDPSessions: 0, UDPSessionLimit: 8, TCPSessions: 3, TCPSessionLimit: 46}
	for _, line := range []struct{ got, want string }{
		{string(appendMemory(nil, &c)), "4096/41943040 bytes (8388608 per session) refused 2 capped 3"},
		{string(appendTimeWait(nil, &c)), "12/8192 tombstones (2048 per session) held 1"},
		{string(appendLookups(nil, &c)), "probes 900 most per segment 4"},
		{string(appendSessions(nil, &c)), "command 1/8 udp 0/8 tcp 3/46"},
	} {
		if line.got != line.want {
			t.Fatalf("got %q, want %q", line.got, line.want)
		}
	}
	// The widest values still fit the command's 160-byte line buffer.
	wide := netapi.TCPCapacity{MemoryBytes: ^uint64(0), MemoryBudget: ^uint32(0), OwnerMemoryBudget: ^uint32(0),
		DropMemory: ^uint64(0), MemoryCapped: ^uint64(0)}
	if size := len(appendMemory(nil, &wide)); size > 160 {
		t.Fatalf("memory line %d bytes", size)
	}
}
