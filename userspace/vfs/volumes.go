package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

// VolumeRoot is where additional volumes are mounted (ADR 0222).
const VolumeRoot = vfs.VolumeRoot

// ValidVolumePath reports whether path may hold an additional volume.
func ValidVolumePath(path []byte) bool { return vfs.ValidVolumePath(path) }

// volumeBackend is one additional volume: a filesystem service instance with
// a file cache of its own, mounted at one path.
type volumeBackend struct {
	remote  Remote
	root    ramfs.NodeID
	path    [vfs.MaxVolumePath]byte
	size    uint8
	mounted bool
}

func (service *Service) volume(slot uint8) *volumeBackend {
	if slot < 1 || slot > vfs.MaxVolumes {
		return nil
	}
	return &service.volumes[slot-1]
}

// AttachVolume connects the filesystem service of an additional volume and
// the file cache it shares with this one. The first attach of a slot mounts
// the volume at path. A later one, after the service was restarted, must
// name the same path and serve the same root. Open handles are revoked.
func (service *Service) AttachVolume(slot uint8, transport Transport, cache []byte, path []byte) vfs.Status {
	volume := service.volume(slot)
	if volume == nil || transport == nil || !ValidVolumePath(path) {
		return vfs.StatusInvalidArgument
	}
	if volume.mounted && string(volume.path[:volume.size]) != string(path) {
		return vfs.StatusAlreadyExists
	}
	if !volume.remote.AttachCache(cache) {
		return vfs.StatusInvalidArgument
	}
	volume.remote.transport = transport
	service.namespace.Invalidate()
	root, status := volume.remote.Root()
	if status != ramfs.StatusOK {
		service.dropVolumeTransport(volume)
		return vfsStatus(status)
	}
	if volume.mounted {
		if root != volume.root {
			service.dropVolumeTransport(volume)
			return vfs.StatusBusy
		}
		service.namespace.InvalidateBackendObjects(&volume.remote)
		return vfs.StatusOK
	}
	if status := service.namespace.Mount(path, &volume.remote, root); status != vfs.StatusOK {
		service.dropVolumeTransport(volume)
		return status
	}
	volume.root, volume.mounted = root, true
	volume.size = uint8(copy(volume.path[:], path))
	return vfs.StatusOK
}

// dropVolumeTransport forgets the service and the cache of a volume. The
// cache bytes are never read again, so a process that still maps them
// cannot hand this service another file's data.
func (service *Service) dropVolumeTransport(volume *volumeBackend) {
	volume.remote.transport = nil
	volume.remote.forgetAll()
	volume.remote.pages = PageCache{}
	service.namespace.Invalidate()
}

// VolumeOffline forgets the filesystem service of slot after it stopped
// answering. The mount stays and answers errors until a replacement attaches
// or the volume is unmounted.
func (service *Service) VolumeOffline(slot uint8) {
	if volume := service.volume(slot); volume != nil {
		service.namespace.InvalidateBackendObjects(&volume.remote)
		service.dropVolumeTransport(volume)
	}
}

// DetachVolume unmounts the volume of slot. Files open through it make it
// busy. Otherwise the service leaves the volume clean on disk first: an ext4
// volume empties its journal and marks its superblock clean. When it cannot,
// the mount stays, offline, and the error goes back; the next detach
// unmounts it without a clean volume behind it. A volume whose service is
// gone is unmounted all the same, so an administrator can always remove it.
func (service *Service) DetachVolume(slot uint8) vfs.Status {
	volume := service.volume(slot)
	if volume == nil {
		return vfs.StatusInvalidArgument
	}
	if !volume.mounted {
		return vfs.StatusNotFound
	}
	if status := service.namespace.BackendInUse(&volume.remote); status != vfs.StatusOK {
		return status
	}
	if volume.remote.transport != nil && volume.remote.Unmount() != ramfs.StatusOK {
		service.VolumeOffline(slot)
		return vfs.StatusBackendError
	}
	if status := service.namespace.UnmountBackend(&volume.remote); status != vfs.StatusOK {
		return status
	}
	service.dropVolumeTransport(volume)
	*volume = volumeBackend{}
	return vfs.StatusOK
}

// SyncVolumes asks every attached additional volume to make its completed
// writes durable and returns how many refused. An ext4 volume commits the
// operations it holds in memory (ADR 0227).
func (service *Service) SyncVolumes() int {
	failed := 0
	for index := range service.volumes {
		volume := &service.volumes[index]
		if volume.mounted && volume.remote.transport != nil && !volume.remote.Flush() {
			failed++
		}
	}
	return failed
}

// VolumeMounted reports whether slot holds a mounted volume.
func (service *Service) VolumeMounted(slot uint8) bool {
	volume := service.volume(slot)
	return volume != nil && volume.mounted
}

// vfsStatus carries a backend status across to the namespace's statuses.
func vfsStatus(status ramfs.Status) vfs.Status {
	switch status {
	case ramfs.StatusCorrupt:
		return vfs.StatusCorrupt
	case ramfs.StatusUnsupported:
		return vfs.StatusUnsupported
	case ramfs.StatusTimeout:
		return vfs.StatusTimeout
	case ramfs.StatusPeerDied:
		return vfs.StatusPeerDied
	case ramfs.StatusNotFound:
		return vfs.StatusNotFound
	case ramfs.StatusPermissionDenied:
		return vfs.StatusPermissionDenied
	case ramfs.StatusInvalidArgument:
		return vfs.StatusInvalidArgument
	}
	return vfs.StatusBackendError
}
