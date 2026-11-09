//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

// volumeState is the connection to one additional volume's filesystem
// service and the mapping of the file cache PID 1 created for its launch
// (ADR 0222). The owner changes it only at a drained barrier.
type volumeState struct {
	transport   filesystemTransport
	cacheHandle handle.ID
	cacheBase   uint64
	cacheSize   uint64
}

var volumeStates [vfs.MaxVolumes]volumeState

// releaseVolume closes the endpoint and unmaps and closes the cache of a
// slot. The service has already forgotten both.
func releaseVolume(state *volumeState) {
	if state.transport.endpoint != 0 {
		sys.HandleClose(state.transport.endpoint)
	}
	if state.cacheSize != 0 {
		sys.VMUnmap(state.cacheBase, state.cacheSize)
	}
	if state.cacheHandle != 0 {
		sys.HandleClose(state.cacheHandle)
	}
	*state = volumeState{}
}

// processVolumeControl handles the additional volume controls, and reports
// whether message was one.
func processVolumeControl(message ipc.Message) bool {
	var result vfs.Status
	if attach, ok := vfs.DecodeAttachVolume(message); ok {
		result = attachVolume(attach)
	} else if slot, generation, ok := vfs.DecodeDetachVolume(message); ok {
		result = detachVolume(slot, generation)
	} else {
		return false
	}
	reply, encoded := vfs.AddSessionReply(result, message.Correlation)
	if !encoded {
		fatal("VFS_CONTROL_REPLY_FAILED\n")
	}
	send(uint64(controlHandle), reply)
	return true
}

// attachVolume installs a new instance's endpoint and cache in its slot. The
// previous instance's endpoint and cache, if any, are released first: its
// pages are never read again (ADR 0222).
func attachVolume(attach vfs.VolumeAttach) vfs.Status {
	state := &volumeStates[attach.Slot-1]
	service.VolumeOffline(attach.Slot)
	releaseVolume(state)
	base, size, status := sys.MemoryMapAnywhere(attach.Cache, true)
	if status != kcall.StatusOK || size != fsproto.CacheSize {
		if status == kcall.StatusOK {
			sys.VMUnmap(base, size)
		}
		sys.HandleClose(attach.Client)
		sys.HandleClose(attach.Cache)
		return vfs.StatusInvalidArgument
	}
	cache := sys.Window(base, size)
	state.cacheHandle, state.cacheBase, state.cacheSize = attach.Cache, base, size
	state.transport.endpoint = attach.Client
	state.transport.generation = attach.Generation
	// releaseVolume cleared the failure state: the new instance has a cache
	// no earlier one ever mapped, so nothing needs to be fenced.
	state.transport.cache = cache
	path := attach.Path[:attach.PathSize]
	result := service.AttachVolume(attach.Slot, &state.transport, cache, path)
	if result != vfs.StatusOK {
		releaseVolume(state)
		syslog.Warn("volume refused", syslog.Uint("slot", uint64(attach.Slot)), syslog.Uint("status", uint64(result)))
		return result
	}
	mark("VOLUME_ATTACHED_OK\n")
	syslog.Info("mounted", syslog.Uint("slot", uint64(attach.Slot)), syslog.Str("path", string(path)))
	return vfs.StatusOK
}

// detachVolume unmounts the volume of slot if generation still names its
// attachment.
func detachVolume(slot uint8, generation uint64) vfs.Status {
	state := &volumeStates[slot-1]
	if !service.VolumeMounted(slot) || state.transport.generation != generation {
		return vfs.StatusNotFound
	}
	result := service.DetachVolume(slot)
	if result == vfs.StatusOK {
		releaseVolume(state)
		syslog.Info("unmounted", syslog.Uint("slot", uint64(slot)))
	}
	return result
}

func volumesOfflinePending() bool {
	for index := range volumeStates {
		transport := &volumeStates[index].transport
		if transport.endpoint != 0 && !transport.applied && transport.availability.failure() != ramfs.StatusOK {
			return true
		}
	}
	return false
}

// applyVolumesOffline detaches each additional volume whose service failed
// and asks PID 1 to replace it. The mount stays and answers errors.
func applyVolumesOffline() {
	for index := range volumeStates {
		state := &volumeStates[index]
		transport := &state.transport
		if transport.endpoint == 0 || transport.applied || transport.availability.failure() == ramfs.StatusOK {
			continue
		}
		slot := uint8(index + 1)
		generation := transport.generation
		service.VolumeOffline(slot)
		releaseVolume(state)
		state.transport.generation = generation
		state.transport.applied = true
		syslog.Warn("volume service gone", syslog.Uint("slot", uint64(slot)))
		notice, ok := vfs.VolumeLostNotice(slot, generation)
		var size int
		if ok {
			size, ok = ipc.Encode(notice, wire[:])
		}
		if !ok || sys.IPCSend(recoveryHandle, wire[:size]) != kcall.StatusOK {
			syslog.Error("volume replacement not requested", syslog.Uint("slot", uint64(slot)))
		}
	}
}
