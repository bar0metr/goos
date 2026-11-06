package main

import (
	"goos/kernel/kcall"
	"goos/userspace/netapi"
	"goos/userspace/network"
)

// tcpSessionSlots is how many TCP sessions netd serves: the WaitAny room
// the driver, the control channel and the command and UDP sessions leave,
// at most the stack's owners. The stack has room for more owners than one
// WaitAny can watch. The bound is policy: one WaitAny names at most 64
// objects because its ready mask is one 64-bit word (ADR 0111), and netd
// waits on every session at once. OpTCPStatus reports the sessions in use
// against it.
const tcpSessionSlots = min(network.MaxTCPSessions, kcall.WaitAnyMaxObjects-2-network.MaxSessions-network.MaxUDPSessions)

// sessionUse is how many sessions of each kind netd serves now.
type sessionUse struct{ command, udp, tcp int }

// tcpCapacity fills the OpTCPStatus capacity block from the stack's counters
// and budgets and the sessions in use. The session limits are the sizes of netd's
// session tables. A value wider than its wire field saturates.
func tcpCapacity(stats *network.TCPStats, budgets network.TCPBudgets, use sessionUse) netapi.TCPCapacity {
	return netapi.TCPCapacity{
		MemoryBytes:  uint64(max(stats.MemoryBytes, 0)),
		MemoryBudget: saturate32(budgets.Memory), OwnerMemoryBudget: saturate32(budgets.OwnerMemory),
		TimeWait: saturate16(stats.TimeWait), TimeWaitBudget: saturate16(budgets.TimeWait),
		OwnerTimeWaitBudget: saturate16(budgets.OwnerTimeWait), TimeWaitHeld: stats.TimeWaitHeld,
		DropMemory: stats.DropMemory, DropBacklog: stats.DropBacklog, MemoryCapped: stats.MemoryCapped,
		LookupProbes: stats.LookupProbes, MaxSegmentProbes: uint32(min(stats.MaxSegmentProbes, 1<<32-1)),
		CommandSessions: uint8(use.command), CommandSessionLimit: network.MaxSessions,
		UDPSessions: uint8(use.udp), UDPSessionLimit: network.MaxUDPSessions,
		TCPSessions: uint8(use.tcp), TCPSessionLimit: tcpSessionSlots,
	}
}

func saturate32(value int) uint32 { return uint32(min(max(value, 0), 1<<32-1)) }

func saturate16(value int) uint16 { return uint16(min(max(value, 0), 1<<16-1)) }
