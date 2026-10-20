//go:build !goos_userspace || goos_goroutines

package main

import (
	"goos/kernel/ipc"
	"goos/kernel/vfs"
	vfsservice "goos/userspace/vfs"
)

type driverRequestKind uint8

const (
	driverRead driverRequestKind = iota + 1
	driverServe
	driverControl
	driverClose
	driverOffline
)

// driverRequest holds the wire message and resources the owner must retain
// while the pool holds its ticket. Workers never access this table.
type driverRequest struct {
	ticket           uint64
	kind             driverRequestKind
	index            int
	session          uint64
	generation       uint64
	message          ipc.Message
	operation        vfs.Operation
	mapBase, mapSize uint64
}

type driverRequests [vfsservice.ReadQueueCapacity]driverRequest

func (requests *driverRequests) put(request driverRequest) bool {
	if request.ticket == 0 || requests.find(request.ticket) != nil {
		return false
	}
	for index := range requests {
		if requests[index].ticket == 0 {
			requests[index] = request
			return true
		}
	}
	return false
}

func (requests *driverRequests) find(ticket uint64) *driverRequest {
	if ticket != 0 {
		for index := range requests {
			if requests[index].ticket == ticket {
				return &requests[index]
			}
		}
	}
	return nil
}

func (requests *driverRequests) release(ticket uint64) bool {
	if request := requests.find(ticket); request != nil {
		*request = driverRequest{}
		return true
	}
	return false
}
