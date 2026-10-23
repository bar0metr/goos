package main

import (
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
)

const (
	// The runtime poller needs its own wake interest; the owner needs control
	// recovery and completion interests; the single Remote lane may wait on one more.
	concurrentSessionLimit = kcall.WaitAnyMaxObjects - 5
	maxBulkObject          = uint64(64 << 20)
)

// parallelRead accepts only the canonical wire forms whose execution does
// not need paths, metadata, authority walks, or the executable cache slot.
// Everything else retains the serial serve path and its validation hooks.
func parallelRead(message ipc.Message) (vfs.Request, bool) {
	request, valid := vfs.DecodeRequest(message)
	if !valid || request.PathsAttached || request.NameReturn || len(request.Path) != 0 || len(request.Target) != 0 || len(request.Data) != 0 || request.Rights != 0 || request.MountID != 0 {
		return vfs.Request{}, false
	}
	switch request.Operation {
	case vfs.OperationRead:
		valid = message.HandleCount == 0 && request.Offset == 0
	case vfs.OperationReadInto:
		valid = message.HandleCount == 1
	default:
		valid = false
	}
	return request, valid
}

func bulkReadWindow(objectSize, offset uint64, size uint32) (uint64, bool) {
	end := offset + uint64(size)
	return end, objectSize <= maxBulkObject && end >= offset && end <= objectSize
}

func nextSessionGeneration(previous uint64) (uint64, bool) {
	if previous == ^uint64(0) {
		return 0, false
	}
	return previous + 1, true
}

func sameSession(expected, generation, current, currentGeneration uint64) bool {
	return expected != 0 && expected == current && generation != 0 && generation == currentGeneration
}

func completionWakeWritten(count int, status kcall.Status) bool {
	// A full nonblocking stream is already readable, so coalescing another
	// wake cannot lose readiness. No other write failure is ignored.
	return status == kcall.StatusOK && count == 1 || status == kcall.StatusResourceExhausted && count == 0
}
