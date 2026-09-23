//go:build uefi

package main

import (
	"goos/kernel/process"
	"goos/kernel/stream"
)

// The probe uses identities no live process has, so its reservations cannot be
// mistaken for a real process's and its streams are released explicitly.
const (
	quotaProbeID   = process.ID(0xffff)
	quotaProbeRoot = uint64(0xfffff000)
)

// quotaBoundsStreamRegistry proves the property the stream quota exists for.
// Any process may create a stream, so before quotas one of them could take
// every slot the system has and break every later pipeline. The check drives
// the same charge-then-create order StreamCreate uses, against a scratch
// process with an ordinary quota, and requires that the refusal arrives while
// slots remain for everyone else.
//
// It runs in the validation build only, and leaves the registry as it found it.
func quotaBoundsStreamRegistry() bool {
	var scratch process.Process
	if !process.Init(&scratch, quotaProbeID, process.AddressSpace{Root: quotaProbeRoot}) {
		return false
	}
	_, limits := scratch.Quota()
	limit := limits[process.ResourceStreams]
	if limit == 0 || limit >= stream.MaxStreams {
		// A quota at or above the whole table would bound nothing.
		return false
	}
	var taken [stream.MaxStreams]uint64
	held := uint32(0)
	for scratch.Charge(process.ResourceStreams, 1) {
		if held == uint32(len(taken)) {
			return false
		}
		endpoint, id, created := streamRegistry.Create(uint64(quotaProbeID))
		if !created {
			scratch.Release(process.ResourceStreams, 1)
			return false
		}
		endpoint.CloseReader()
		endpoint.CloseWriter()
		taken[held] = id
		held++
	}
	ok := held == limit
	// Slots remain, which is the whole point: another process can still build
	// a pipeline after this one has taken all it may.
	spare, spareID, spareCreated := streamRegistry.Create(0)
	if spareCreated {
		spare.CloseReader()
		spare.CloseWriter()
		if _, released := streamRegistry.Release(spareID); !released {
			ok = false
		}
	} else {
		ok = false
	}
	for index := uint32(0); index < held; index++ {
		if _, released := streamRegistry.Release(taken[index]); !released {
			ok = false
		}
		if !scratch.Release(process.ResourceStreams, 1) {
			ok = false
		}
	}
	if used, _ := scratch.Quota(); used[process.ResourceStreams] != 0 {
		ok = false
	}
	return ok
}
