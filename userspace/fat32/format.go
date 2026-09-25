package fat32

// Layout describes a volume Format writes.
type Layout struct {
	// SectorsPerCluster is a power of two from 1 to 128.
	SectorsPerCluster uint8
	// FATs is the number of FAT copies, 1 or 2.
	FATs uint8
	// NoMirror disables FAT mirroring and makes ActiveFAT the only copy in
	// use, as the extended-flags field allows.
	NoMirror  bool
	ActiveFAT uint8
}

const (
	formatReserved   = 32
	formatInfoSector = 1
	formatBackupBoot = 6
	formatMedia      = 0xf8
)

// Format writes an empty FAT32 volume over the whole device: a boot sector
// and its backup, an FSInfo sector (no backup of it: nothing reads one, and
// the reserved sectors after the backup boot sector stay unused), zeroed FATs whose first entries hold the
// media byte and the root directory's end of chain, and a zeroed root
// directory cluster. The FAT is sized for exactly the clusters that remain
// after the FATs themselves.
func Format(device Device, layout Layout) bool {
	perCluster := uint64(layout.SectorsPerCluster)
	copies := uint64(layout.FATs)
	if device == nil || perCluster == 0 || perCluster > maxSectorsPerCluster || perCluster&(perCluster-1) != 0 ||
		copies == 0 || copies > maxFATs || layout.NoMirror && uint64(layout.ActiveFAT) >= copies {
		return false
	}
	capacity := device.Capacity()
	if capacity%sectorSize != 0 || capacity/sectorSize > uint64(^uint32(0)) {
		return false
	}
	total := capacity / sectorSize
	fatSectors := uint64(1)
	clusters := uint64(0)
	for {
		metadata := formatReserved + copies*fatSectors
		if total <= metadata+perCluster {
			return false
		}
		clusters = (total - metadata) / perCluster
		needed := ((clusters+2)*4 + sectorSize - 1) / sectorSize
		if needed <= fatSectors {
			break
		}
		fatSectors = needed
	}
	if clusters > maxClusters {
		return false
	}
	var boot [sectorSize]byte
	boot[0], boot[1], boot[2] = 0xeb, 0x58, 0x90
	copy(boot[3:11], "GOOSFAT ")
	put16(boot[11:13], uint16(sectorSize))
	boot[13] = layout.SectorsPerCluster
	put16(boot[14:16], formatReserved)
	boot[16] = layout.FATs
	boot[21] = formatMedia
	put32(boot[32:36], uint32(total))
	put32(boot[36:40], uint32(fatSectors))
	if layout.NoMirror {
		put16(boot[40:42], extNoMirror|uint16(layout.ActiveFAT))
	}
	put32(boot[44:48], 2)
	put16(boot[48:50], formatInfoSector)
	put16(boot[50:52], formatBackupBoot)
	boot[64], boot[66] = 0x80, 0x29
	copy(boot[71:82], "GOOS       ")
	copy(boot[82:90], "FAT32   ")
	boot[510], boot[511] = 0x55, 0xaa
	var info [sectorSize]byte
	put32(info[0:4], fsInfoLead)
	put32(info[484:488], fsInfoStruct)
	put32(info[fsInfoFreeCount:], uint32(clusters-1))
	put32(info[fsInfoNextFree:], 3)
	put32(info[508:512], fsInfoTrail)
	if !device.WriteAt(0, boot[:]) || !device.WriteAt(formatBackupBoot*sectorSize, boot[:]) ||
		!device.WriteAt(formatInfoSector*sectorSize, info[:]) {
		return false
	}
	var zero [4096]byte
	fatBytes := fatSectors * sectorSize
	for index := uint64(0); index < copies; index++ {
		start := (formatReserved + index*fatSectors) * sectorSize
		for done := uint64(0); done < fatBytes; {
			chunk := uint64(len(zero))
			if chunk > fatBytes-done {
				chunk = fatBytes - done
			}
			if !device.WriteAt(start+done, zero[:chunk]) {
				return false
			}
			done += chunk
		}
		var head [12]byte
		put32(head[0:4], 0x0fffff00|formatMedia)
		put32(head[4:8], endOfChainMark)
		put32(head[8:12], endOfChainMark)
		if !device.WriteAt(start, head[:]) {
			return false
		}
	}
	root := (formatReserved + copies*fatSectors) * sectorSize
	for done := uint64(0); done < perCluster*sectorSize; done += sectorSize {
		if !device.WriteAt(root+done, zero[:sectorSize]) {
			return false
		}
	}
	return device.Flush()
}
