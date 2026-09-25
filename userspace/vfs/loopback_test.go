package vfsservice

import (
	"goos/kernel/ipc"
	"goos/userspace/fat32"
	"goos/userspace/fsproto"
)

// loopback is a filesystem service run in the test process. Every exchange
// goes through the protocol's wire encoding, as it does between processes, so
// the tests exercise the same bytes the FAT32 service sees.
type loopback struct {
	volume      fat32.Volume
	correlation uint64
	scratch     [fsproto.MaxReplyData]byte
	// fail, when set, makes every exchange fail from then on, as a service
	// that has died does.
	fail bool
	// corrupt, when set, damages each reply after it is encoded.
	corrupt bool
}

func newLoopback(device fat32.Device) *loopback {
	service := &loopback{}
	service.volume.Mount(device)
	return service
}

func (service *loopback) Exchange(request fsproto.Request) (fsproto.Reply, bool) {
	if service.fail {
		return fsproto.Reply{}, false
	}
	service.correlation++
	var message ipc.Message
	if !fsproto.EncodeRequest(request, service.correlation, &message) {
		return fsproto.Reply{}, false
	}
	decoded, ok := fsproto.DecodeRequest(&message)
	if !ok {
		return fsproto.Reply{}, false
	}
	var answer ipc.Message
	if !fsproto.EncodeReply(fsproto.Serve(&service.volume, decoded, &service.scratch), message.Correlation, &answer) {
		return fsproto.Reply{}, false
	}
	if service.corrupt {
		answer.Payload[6] = 1
	}
	reply, ok := fsproto.DecodeReply(&answer)
	return reply, ok && answer.Correlation == service.correlation
}
