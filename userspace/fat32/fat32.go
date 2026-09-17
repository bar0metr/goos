package fat32

import "goos/kernel/ramfs"

const (
	sectorSize  = uint64(512)
	rootNode    = ramfs.NodeID(1)
	maxFileSize = uint64(64 * 1024)
)

type Device interface {
	ReadAt(offset uint64, target []byte) bool
	WriteAt(offset uint64, data []byte) bool
	Flush() bool
}

type FileSystem struct {
	device        Device
	fatStart      uint64
	dataStart     uint64
	totalClusters uint32
	rootCluster   uint32
	sector        [512]byte
}

func (fs *FileSystem) Mount(device Device) (ramfs.NodeID, bool) {
	if device == nil || !device.ReadAt(0, fs.sector[:]) || fs.sector[510] != 0x55 || fs.sector[511] != 0xaa || get16(fs.sector[11:13]) != 512 || fs.sector[13] != 1 || fs.sector[16] != 1 {
		return 0, false
	}
	reserved := uint64(get16(fs.sector[14:16]))
	fatSectors := uint64(get32(fs.sector[36:40]))
	total := uint64(get32(fs.sector[32:36]))
	root := get32(fs.sector[44:48])
	if reserved < 2 || fatSectors == 0 || total <= reserved+fatSectors || root < 2 {
		return 0, false
	}
	fs.device, fs.fatStart, fs.dataStart = device, reserved*sectorSize, (reserved+fatSectors)*sectorSize
	fs.totalClusters, fs.rootCluster = uint32(total-reserved-fatSectors), root
	return rootNode, true
}

func (fs *FileSystem) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	if parent != rootNode {
		return 0, ramfs.StatusNotDirectory
	}
	encoded, ok := encodeName(name)
	if !ok {
		return 0, ramfs.StatusInvalidArgument
	}
	index, _, found := fs.find(encoded)
	if !found {
		return 0, ramfs.StatusNotFound
	}
	return ramfs.NodeID(index + 2), ramfs.StatusOK
}

func (fs *FileSystem) CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	if parent != rootNode {
		return 0, ramfs.StatusNotDirectory
	}
	encoded, ok := encodeName(name)
	if !ok {
		return 0, ramfs.StatusInvalidArgument
	}
	if _, _, found := fs.find(encoded); found {
		return 0, ramfs.StatusAlreadyExists
	}
	index, entry, ok := fs.freeEntry()
	if !ok {
		return 0, ramfs.StatusResourceExhausted
	}
	copy(entry[:11], encoded[:])
	entry[11] = 0x20
	if !fs.writeEntry(index, entry) || !fs.device.Flush() {
		return 0, ramfs.StatusResourceExhausted
	}
	return ramfs.NodeID(index + 2), ramfs.StatusOK
}

func (fs *FileSystem) MkdirName(ramfs.NodeID, ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	return 0, ramfs.StatusResourceExhausted
}

func (fs *FileSystem) Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status) {
	entry, status := fs.fileEntry(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	size := uint64(get32(entry[28:32]))
	if offset >= size {
		return 0, ramfs.StatusOK
	}
	count := uint64(len(target))
	if count > size-offset {
		count = size - offset
	}
	cluster := entryCluster(entry)
	var ok bool
	cluster, ok = fs.advance(cluster, uint32(offset/sectorSize))
	if !ok {
		return 0, ramfs.StatusResourceExhausted
	}
	done, within := uint64(0), offset%sectorSize
	for done < count {
		if !fs.device.ReadAt(fs.clusterOffset(cluster), fs.sector[:]) {
			return int(done), ramfs.StatusResourceExhausted
		}
		chunk := count - done
		if chunk > sectorSize-within {
			chunk = sectorSize - within
		}
		copy(target[done:done+chunk], fs.sector[within:within+chunk])
		done += chunk
		within = 0
		if done < count {
			cluster, ok = fs.next(cluster)
			if !ok {
				return int(done), ramfs.StatusResourceExhausted
			}
		}
	}
	return int(done), ramfs.StatusOK
}

func (fs *FileSystem) Write(id ramfs.NodeID, offset uint64, data []byte) (int, ramfs.Status) {
	entry, status := fs.fileEntry(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if offset > maxFileSize || uint64(len(data)) > maxFileSize-offset {
		return 0, ramfs.StatusResourceExhausted
	}
	needed := uint32((offset + uint64(len(data)) + sectorSize - 1) / sectorSize)
	cluster := entryCluster(entry)
	if needed != 0 && cluster == 0 {
		cluster = fs.allocate()
		if cluster == 0 {
			return 0, ramfs.StatusResourceExhausted
		}
		setEntryCluster(&entry, cluster)
	}
	if !fs.ensure(cluster, needed) {
		return 0, ramfs.StatusResourceExhausted
	}
	cluster, _ = fs.advance(cluster, uint32(offset/sectorSize))
	done, within := uint64(0), offset%sectorSize
	for done < uint64(len(data)) {
		if (within != 0 || uint64(len(data))-done < sectorSize) && !fs.device.ReadAt(fs.clusterOffset(cluster), fs.sector[:]) {
			return int(done), ramfs.StatusResourceExhausted
		}
		chunk := uint64(len(data)) - done
		if chunk > sectorSize-within {
			chunk = sectorSize - within
		}
		copy(fs.sector[within:within+chunk], data[done:done+chunk])
		if !fs.device.WriteAt(fs.clusterOffset(cluster), fs.sector[:]) {
			return int(done), ramfs.StatusResourceExhausted
		}
		done += chunk
		within = 0
		if done < uint64(len(data)) {
			cluster, _ = fs.next(cluster)
		}
	}
	size := uint64(get32(entry[28:32]))
	if offset+done > size {
		put32(entry[28:32], uint32(offset+done))
	}
	if !fs.writeEntry(int(id-2), entry) || !fs.device.Flush() {
		return int(done), ramfs.StatusResourceExhausted
	}
	return int(done), ramfs.StatusOK
}

func (fs *FileSystem) Truncate(id ramfs.NodeID, size uint64) ramfs.Status {
	entry, status := fs.fileEntry(id)
	if status != ramfs.StatusOK {
		return status
	}
	if size > uint64(get32(entry[28:32])) {
		return ramfs.StatusInvalidArgument
	}
	if size == 0 {
		fs.freeChain(entryCluster(entry))
		setEntryCluster(&entry, 0)
	}
	put32(entry[28:32], uint32(size))
	if !fs.writeEntry(int(id-2), entry) || !fs.device.Flush() {
		return ramfs.StatusResourceExhausted
	}
	return ramfs.StatusOK
}

func (fs *FileSystem) Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status) {
	if id == rootNode {
		return ramfs.Stat{ID: id, Kind: ramfs.KindDirectory}, ramfs.StatusOK
	}
	entry, status := fs.fileEntry(id)
	if status != ramfs.StatusOK {
		return ramfs.Stat{}, status
	}
	return ramfs.Stat{ID: id, Kind: ramfs.KindFile, Size: uint64(get32(entry[28:32]))}, ramfs.StatusOK
}

func (fs *FileSystem) ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status) {
	if parent != rootNode || cursor < 0 {
		return ramfs.Entry{}, cursor, ramfs.StatusInvalidArgument
	}
	for index := cursor; index < 16; index++ {
		entry, ok := fs.readEntry(index)
		if !ok {
			return ramfs.Entry{}, cursor, ramfs.StatusResourceExhausted
		}
		if entry[0] == 0 {
			return ramfs.Entry{}, cursor, ramfs.StatusNotFound
		}
		if entry[0] == 0xe5 || entry[11]&0x18 != 0 {
			continue
		}
		name, size := decodeName(entry[:11])
		result := ramfs.Entry{Stat: ramfs.Stat{ID: ramfs.NodeID(index + 2), Kind: ramfs.KindFile, Size: uint64(get32(entry[28:32]))}, Size: uint8(size)}
		copy(result.Name[:], name[:size])
		return result, index + 1, ramfs.StatusOK
	}
	return ramfs.Entry{}, cursor, ramfs.StatusNotFound
}

func (fs *FileSystem) RenameName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	if parent != rootNode {
		return ramfs.StatusNotDirectory
	}
	encoded, ok := encodeName(name)
	if !ok {
		return ramfs.StatusInvalidArgument
	}
	if _, _, found := fs.find(encoded); found {
		return ramfs.StatusAlreadyExists
	}
	entry, status := fs.fileEntry(id)
	if status != ramfs.StatusOK {
		return status
	}
	copy(entry[:11], encoded[:])
	if !fs.writeEntry(int(id-2), entry) || !fs.device.Flush() {
		return ramfs.StatusResourceExhausted
	}
	return ramfs.StatusOK
}
func (fs *FileSystem) UnlinkName(parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	id, status := fs.OpenName(parent, name)
	if status != ramfs.StatusOK {
		return status
	}
	entry, _ := fs.fileEntry(id)
	fs.freeChain(entryCluster(entry))
	entry[0] = 0xe5
	if !fs.writeEntry(int(id-2), entry) || !fs.device.Flush() {
		return ramfs.StatusResourceExhausted
	}
	return ramfs.StatusOK
}

func (fs *FileSystem) rootOffset() uint64 { return fs.clusterOffset(fs.rootCluster) }
func (fs *FileSystem) clusterOffset(cluster uint32) uint64 {
	return fs.dataStart + uint64(cluster-2)*sectorSize
}
func (fs *FileSystem) readEntry(index int) ([32]byte, bool) {
	var entry [32]byte
	if index < 0 || index >= 16 || !fs.device.ReadAt(fs.rootOffset()+uint64(index*32), entry[:]) {
		return entry, false
	}
	return entry, true
}
func (fs *FileSystem) writeEntry(index int, entry [32]byte) bool {
	return index >= 0 && index < 16 && fs.device.WriteAt(fs.rootOffset()+uint64(index*32), entry[:])
}
func (fs *FileSystem) fileEntry(id ramfs.NodeID) ([32]byte, ramfs.Status) {
	if id < 2 || id >= 18 {
		return [32]byte{}, ramfs.StatusNotFound
	}
	entry, ok := fs.readEntry(int(id - 2))
	if !ok || entry[0] == 0 || entry[0] == 0xe5 {
		return [32]byte{}, ramfs.StatusNotFound
	}
	return entry, ramfs.StatusOK
}
func (fs *FileSystem) find(name [11]byte) (int, [32]byte, bool) {
	for i := 0; i < 16; i++ {
		entry, ok := fs.readEntry(i)
		if !ok {
			return 0, entry, false
		}
		if entry[0] == 0 {
			return 0, entry, false
		}
		same := true
		for j := 0; j < 11; j++ {
			if entry[j] != name[j] {
				same = false
			}
		}
		if entry[0] != 0xe5 && same {
			return i, entry, true
		}
	}
	return 0, [32]byte{}, false
}
func (fs *FileSystem) freeEntry() (int, [32]byte, bool) {
	for i := 0; i < 16; i++ {
		entry, ok := fs.readEntry(i)
		if !ok {
			return 0, entry, false
		}
		if entry[0] == 0 || entry[0] == 0xe5 {
			return i, [32]byte{}, true
		}
	}
	return 0, [32]byte{}, false
}
func (fs *FileSystem) fat(cluster uint32) (uint32, bool) {
	var data [4]byte
	if cluster >= fs.totalClusters+2 || !fs.device.ReadAt(fs.fatStart+uint64(cluster)*4, data[:]) {
		return 0, false
	}
	return get32(data[:]) & 0x0fffffff, true
}
func (fs *FileSystem) setFAT(cluster, value uint32) bool {
	var data [4]byte
	put32(data[:], value)
	return fs.device.WriteAt(fs.fatStart+uint64(cluster)*4, data[:])
}
func (fs *FileSystem) next(cluster uint32) (uint32, bool) {
	value, ok := fs.fat(cluster)
	return value, ok && value >= 2 && value < 0x0ffffff8
}
func (fs *FileSystem) advance(cluster uint32, count uint32) (uint32, bool) {
	if cluster < 2 {
		return 0, false
	}
	for i := uint32(0); i < count; i++ {
		var ok bool
		cluster, ok = fs.next(cluster)
		if !ok {
			return 0, false
		}
	}
	return cluster, true
}
func (fs *FileSystem) allocate() uint32 {
	for cluster := uint32(3); cluster < fs.totalClusters+2; cluster++ {
		value, ok := fs.fat(cluster)
		if !ok {
			return 0
		}
		if value == 0 {
			if !fs.setFAT(cluster, 0x0fffffff) {
				return 0
			}
			for i := range fs.sector {
				fs.sector[i] = 0
			}
			if !fs.device.WriteAt(fs.clusterOffset(cluster), fs.sector[:]) {
				return 0
			}
			return cluster
		}
	}
	return 0
}
func (fs *FileSystem) ensure(first uint32, count uint32) bool {
	if count == 0 {
		return true
	}
	cluster := first
	for i := uint32(1); i < count; i++ {
		next, ok := fs.next(cluster)
		if !ok {
			next = fs.allocate()
			if next == 0 || !fs.setFAT(cluster, next) {
				return false
			}
		}
		cluster = next
	}
	return true
}
func (fs *FileSystem) freeChain(cluster uint32) {
	for cluster >= 2 {
		next, ok := fs.fat(cluster)
		fs.setFAT(cluster, 0)
		if !ok || next >= 0x0ffffff8 {
			break
		}
		cluster = next
	}
}
func entryCluster(entry [32]byte) uint32 {
	return uint32(get16(entry[20:22]))<<16 | uint32(get16(entry[26:28]))
}
func setEntryCluster(entry *[32]byte, cluster uint32) {
	put16(entry[20:22], uint16(cluster>>16))
	put16(entry[26:28], uint16(cluster))
}

func encodeName(name ramfs.Name) ([11]byte, bool) {
	var raw [ramfs.MaxName]byte
	n := name.CopyTo(raw[:])
	var out [11]byte
	for i := range out {
		out[i] = ' '
	}
	if n == 0 {
		return out, false
	}
	dot := -1
	for i := 0; i < n; i++ {
		if raw[i] == '.' {
			if dot != -1 {
				return out, false
			}
			dot = i
		}
	}
	base := n
	ext := 0
	if dot >= 0 {
		base = dot
		ext = n - dot - 1
	}
	if base < 1 || base > 8 || ext > 3 {
		return out, false
	}
	for i := 0; i < base; i++ {
		c, ok := upper(raw[i])
		if !ok {
			return out, false
		}
		out[i] = c
	}
	for i := 0; i < ext; i++ {
		c, ok := upper(raw[dot+1+i])
		if !ok {
			return out, false
		}
		out[8+i] = c
	}
	return out, true
}
func decodeName(raw []byte) ([ramfs.MaxName]byte, int) {
	var out [ramfs.MaxName]byte
	n := 0
	for i := 0; i < 8 && raw[i] != ' '; i++ {
		out[n] = lower(raw[i])
		n++
	}
	ext := 0
	for ext < 3 && raw[8+ext] != ' ' {
		ext++
	}
	if ext > 0 {
		out[n] = '.'
		n++
		for i := 0; i < ext; i++ {
			out[n] = lower(raw[8+i])
			n++
		}
	}
	return out, n
}
func upper(value byte) (byte, bool) {
	if value >= 'a' && value <= 'z' {
		value -= 32
	}
	return value, value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_' || value == '-'
}
func lower(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + 32
	}
	return value
}
func get16(data []byte) uint16 { return uint16(data[0]) | uint16(data[1])<<8 }
func get32(data []byte) uint32 {
	return uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
}
func put16(data []byte, value uint16) { data[0] = byte(value); data[1] = byte(value >> 8) }
func put32(data []byte, value uint32) {
	for i := 0; i < 4; i++ {
		data[i] = byte(value >> uint(i*8))
	}
}
