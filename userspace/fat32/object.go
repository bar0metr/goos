package fat32

import "goos/kernel/ramfs"

type liveObject struct {
	location   ramfs.NodeID
	generation uint32
	references uint16
	used       bool
}

func (fs *FileSystem) AcquireObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	if uint64(id)&liveObjectFlag != 0 {
		return 0, ramfs.StatusInvalidArgument
	}
	if _, _, _, status := fs.fileNode(id); status != ramfs.StatusOK {
		return 0, status
	}
	for index := range fs.objects {
		object := &fs.objects[index]
		if object.used && object.location == id {
			if object.references == ^uint16(0) {
				return 0, ramfs.StatusResourceExhausted
			}
			object.references++
			return objectCookie(index, object.generation), ramfs.StatusOK
		}
	}
	for index := range fs.objects {
		object := &fs.objects[index]
		if object.used {
			continue
		}
		if object.generation == 0 || object.generation >= 0x7fffffff {
			object.generation = 1
		}
		object.location, object.references, object.used = id, 1, true
		return objectCookie(index, object.generation), ramfs.StatusOK
	}
	return 0, ramfs.StatusResourceExhausted
}

func (fs *FileSystem) ReleaseObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	index, generation, ok := decodeObject(id)
	if !ok {
		return 0, ramfs.StatusNotFound
	}
	object := &fs.objects[index]
	if !object.used || object.generation != generation {
		return 0, ramfs.StatusNotFound
	}
	if object.references > 1 {
		object.references--
		return id, ramfs.StatusOK
	}
	location := object.location
	object.location, object.references, object.used = 0, 0, false
	object.generation++
	if object.generation == 0 || object.generation >= 0x7fffffff {
		object.generation = 1
	}
	return location, ramfs.StatusOK
}

func (fs *FileSystem) objectLocation(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	if uint64(id)&liveObjectFlag == 0 {
		return id, ramfs.StatusOK
	}
	index, generation, ok := decodeObject(id)
	if !ok {
		return 0, ramfs.StatusNotFound
	}
	object := &fs.objects[index]
	if !object.used || object.generation != generation {
		return 0, ramfs.StatusNotFound
	}
	return object.location, ramfs.StatusOK
}

func (fs *FileSystem) moveObjects(from, to ramfs.NodeID) {
	for index := range fs.objects {
		if object := &fs.objects[index]; object.used && object.location == from {
			object.location = to
		}
	}
}

func (fs *FileSystem) objectOpen(location ramfs.NodeID) bool {
	for index := range fs.objects {
		if object := &fs.objects[index]; object.used && object.location == location {
			return true
		}
	}
	return false
}

func (fs *FileSystem) resetObjects() {
	for index := range fs.objects {
		object := &fs.objects[index]
		if object.used {
			object.generation++
		}
		if object.generation == 0 || object.generation >= 0x7fffffff {
			object.generation = 1
		}
		object.location, object.references, object.used = 0, 0, false
	}
}

func objectCookie(index int, generation uint32) ramfs.NodeID {
	return ramfs.NodeID(liveObjectFlag | uint64(generation)<<16 | uint64(index+1))
}

func decodeObject(id ramfs.NodeID) (int, uint32, bool) {
	value := uint64(id)
	index := int(value&0xffff) - 1
	generation := uint32(value>>16) & 0x7fffffff
	return index, generation, value&liveObjectFlag != 0 && index >= 0 && index < liveObjectSlots && generation != 0
}
