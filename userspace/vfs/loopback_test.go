package vfsservice

import (
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
	"goos/userspace/fat32"
	"goos/userspace/fsproto"
)

// testCache is the file cache the tests' VFS and loopback services share, as
// the real ones share a mapping of one shared-memory object.
var testCache = make([]byte, fsproto.CacheSize)

// loopback is a filesystem service run in the test process. Every exchange
// goes through the protocol's wire encoding, as it does between processes, so
// the tests exercise the same bytes the FAT32 service sees.
type loopback struct {
	volume      fat32.Volume
	correlation uint64
	name        [ramfs.MaxName]byte
	requested   [ramfs.MaxName]byte
	replied     [ramfs.MaxName]byte
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

func (service *loopback) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	if service.fail {
		return fsproto.Reply{}, ramfs.StatusPeerDied
	}
	service.correlation++
	var message ipc.Message
	if !fsproto.EncodeRequest(request, service.correlation, &message, testCache) {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	decoded, ok := fsproto.DecodeRequest(&message, testCache, &service.requested)
	if !ok {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	var answer ipc.Message
	if !fsproto.EncodeReply(fsproto.Serve(&service.volume, decoded, testCache, &service.name), message.Correlation, &answer, testCache) {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	if service.corrupt {
		answer.Payload[5] = 1
	}
	reply, ok := fsproto.DecodeReply(&answer, testCache, &service.replied)
	if !ok || answer.Correlation != service.correlation {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	return reply, ramfs.StatusOK
}
