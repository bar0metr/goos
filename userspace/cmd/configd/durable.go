//go:build goos_userspace

package main

import (
	"goos/internal/limits"
	"goos/kernel/vfs"
	"goos/userspace/config"
)

var durableBuffer [vfs.MaxProtocolData]byte
var durableTemporary [limits.PathMax]byte
var durablePending bool

func persistDurable(candidate *config.Store) config.Status {
	status, stage := config.PersistStore(configFiles{}, []byte(configPath), candidate, durableBuffer[:], &durableTemporary)
	result := config.PersistenceStatus(status, stage)
	if result == config.StatusOK {
		persistFailed, durablePending = false, false
	} else if result == config.StatusPersistUnknown {
		// Keep the tentative store for a forward retry on session renewal.
		persistFailed, durablePending = true, true
	}
	return result
}

type configFiles struct{}

// request keeps no reply data beyond the reply it returns.
func (configFiles) request(request vfs.Request) vfs.Reply {
	reply, received := callVFS(request)
	if !received {
		return vfs.Reply{Status: vfs.StatusPeerDied}
	}
	return reply
}

func (files configFiles) Create(path []byte) vfs.Status {
	return files.request(vfs.Request{Operation: vfs.OperationCreate, Path: path}).Status
}
func (files configFiles) Unlink(path []byte) vfs.Status {
	return files.request(vfs.Request{Operation: vfs.OperationUnlink, Path: path}).Status
}
func (files configFiles) OpenWrite(path []byte) (vfs.Handle, vfs.Status) {
	reply := files.request(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: path})
	return reply.Handle, reply.Status
}
func (files configFiles) Write(file vfs.Handle, data []byte) (int, vfs.Status) {
	reply := files.request(vfs.Request{Operation: vfs.OperationWrite, Handle: file, Data: data})
	return int(reply.Size), reply.Status
}
func (files configFiles) Close(file vfs.Handle) vfs.Status {
	return files.request(vfs.Request{Operation: vfs.OperationClose, Handle: file}).Status
}
func (files configFiles) Replace(source, target []byte) vfs.Status {
	return files.request(vfs.Request{Operation: vfs.OperationReplace, Path: source, Target: target}).Status
}
