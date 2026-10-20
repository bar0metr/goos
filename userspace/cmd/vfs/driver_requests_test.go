//go:build !goos_userspace || goos_goroutines

package main

import (
	"goos/kernel/handle"
	"testing"
)

func TestDriverRequestsKeepAttachmentsAndMappingsUntilRelease(t *testing.T) {
	requests := &driverRequests{}
	request := driverRequest{ticket: 1, kind: driverRead, index: 2, session: 7, generation: 3, mapBase: 4096, mapSize: 8192}
	request.message.HandleCount, request.message.Handles[0] = 1, handle.ID(11)
	if !requests.put(request) || requests.put(request) || requests.put(driverRequest{}) {
		t.Fatal("duplicate or zero ticket admitted")
	}
	got := requests.find(1)
	if got == nil || got.mapBase != 4096 || got.mapSize != 8192 || got.message.Handles[0] != 11 || got.generation != 3 {
		t.Fatal("request ownership lost")
	}
	if requests.release(2) || !requests.release(1) || requests.release(1) || requests.find(1) != nil {
		t.Fatal("release was not ticket-specific and exactly once")
	}
	if !requests.put(driverRequest{ticket: 2, generation: 4}) || requests.release(1) {
		t.Fatal("stale ticket released reused record")
	}
}

func TestDriverRequestsAreBoundedLikePoolIngress(t *testing.T) {
	requests := &driverRequests{}
	for index := range requests {
		if !requests.put(driverRequest{ticket: uint64(index + 1)}) {
			t.Fatal("capacity smaller than pool")
		}
	}
	if requests.put(driverRequest{ticket: uint64(len(requests) + 1)}) {
		t.Fatal("unbounded request metadata")
	}
}
