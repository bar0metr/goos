package main

import "goos/userspace/netapi"

// The capacity lines of net status: what each TCP budget and session table
// holds against its limit. Each appends to target and returns it.

func appendMemory(target []byte, c *netapi.TCPCapacity) []byte {
	target = appendUse(target, c.MemoryBytes, uint64(c.MemoryBudget))
	target = append(target, " bytes ("...)
	target = appendDecimal(target, uint64(c.OwnerMemoryBudget))
	target = append(target, " per session) refused "...)
	target = appendDecimal(target, c.DropMemory)
	target = append(target, " capped "...)
	return appendDecimal(target, c.MemoryCapped)
}

func appendTimeWait(target []byte, c *netapi.TCPCapacity) []byte {
	target = appendUse(target, uint64(c.TimeWait), uint64(c.TimeWaitBudget))
	target = append(target, " tombstones ("...)
	target = appendDecimal(target, uint64(c.OwnerTimeWaitBudget))
	target = append(target, " per session) held "...)
	return appendDecimal(target, c.TimeWaitHeld)
}

func appendLookups(target []byte, c *netapi.TCPCapacity) []byte {
	target = append(target, "probes "...)
	target = appendDecimal(target, c.LookupProbes)
	target = append(target, " most per segment "...)
	return appendDecimal(target, uint64(c.MaxSegmentProbes))
}

func appendSessions(target []byte, c *netapi.TCPCapacity) []byte {
	target = append(target, "command "...)
	target = appendUse(target, uint64(c.CommandSessions), uint64(c.CommandSessionLimit))
	target = append(target, " udp "...)
	target = appendUse(target, uint64(c.UDPSessions), uint64(c.UDPSessionLimit))
	target = append(target, " tcp "...)
	return appendUse(target, uint64(c.TCPSessions), uint64(c.TCPSessionLimit))
}

func appendUse(target []byte, used, limit uint64) []byte {
	target = appendDecimal(target, used)
	target = append(target, '/')
	return appendDecimal(target, limit)
}

func appendDecimal(target []byte, value uint64) []byte {
	if value >= 10 {
		target = appendDecimal(target, value/10)
	}
	return append(target, byte('0'+value%10))
}
