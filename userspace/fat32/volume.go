package fat32

import "goos/kernel/ramfs"

// Volume is a FileSystem bound to the device it is mounted from: the shape the
// FAT32 filesystem service serves over its protocol (ADR 0096). Until Mount
// has accepted the volume, every operation is refused with an I/O error; the
// filesystem code never runs against a volume it has not checked.
type Volume struct {
	fs      FileSystem
	device  Device
	root    ramfs.NodeID
	mounted bool
}

// Mount checks the volume on device and remembers the result.
func (volume *Volume) Mount(device Device) bool {
	volume.device = device
	volume.root, volume.mounted = volume.fs.Mount(device)
	return volume.mounted
}

// Root reports the root of the mounted volume, or that the volume was refused.
func (volume *Volume) Root() (ramfs.NodeID, ramfs.Status) {
	if !volume.mounted {
		return 0, ramfs.StatusIOError
	}
	return volume.root, ramfs.StatusOK
}

// Flush makes every completed write durable on the device. It does not need
// an accepted volume: shutting down flushes the device either way.
func (volume *Volume) Flush() bool {
	if volume.mounted {
		return volume.fs.Flush()
	}
	return volume.device != nil && volume.device.Flush()
}

func (volume *Volume) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	if !volume.mounted {
		return 0, ramfs.StatusIOError
	}
	return volume.fs.OpenName(parent, name)
}

func (volume *Volume) CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	if !volume.mounted {
		return 0, ramfs.StatusIOError
	}
	return volume.fs.CreateName(parent, name)
}

func (volume *Volume) MkdirName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	if !volume.mounted {
		return 0, ramfs.StatusIOError
	}
	return volume.fs.MkdirName(parent, name)
}

func (volume *Volume) Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status) {
	if !volume.mounted {
		return 0, ramfs.StatusIOError
	}
	return volume.fs.Read(id, offset, target)
}

func (volume *Volume) Write(id ramfs.NodeID, offset uint64, data []byte) (int, ramfs.Status) {
	if !volume.mounted {
		return 0, ramfs.StatusIOError
	}
	return volume.fs.Write(id, offset, data)
}

func (volume *Volume) Truncate(id ramfs.NodeID, size uint64) ramfs.Status {
	if !volume.mounted {
		return ramfs.StatusIOError
	}
	return volume.fs.Truncate(id, size)
}

func (volume *Volume) Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status) {
	if !volume.mounted {
		return ramfs.Stat{}, ramfs.StatusIOError
	}
	return volume.fs.Stat(id)
}

func (volume *Volume) ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status) {
	if !volume.mounted {
		return ramfs.Entry{}, 0, ramfs.StatusIOError
	}
	return volume.fs.ReadDir(parent, cursor)
}

func (volume *Volume) RenameName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	if !volume.mounted {
		return ramfs.StatusIOError
	}
	return volume.fs.RenameName(id, parent, name)
}

func (volume *Volume) UnlinkName(parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	if !volume.mounted {
		return ramfs.StatusIOError
	}
	return volume.fs.UnlinkName(parent, name)
}
