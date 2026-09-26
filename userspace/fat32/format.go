package fat32

// Layout describes a volume Format writes.
type Layout struct {
	// BytesPerSector is the logical sector size: 512, 1024, 2048 or 4096.
	// Zero means 512.
	BytesPerSector uint16
	// SectorsPerCluster is a power of two from 1 to 128.
	SectorsPerCluster uint8
	// FATs is the number of FAT copies, 1 or 2.
	FATs uint8
	// NoMirror disables FAT mirroring and makes ActiveFAT the only copy in
	// use, as the extended-flags field allows.
	NoMirror  bool
	ActiveFAT uint8
	// VolumeID is the serial number in the extended BPB. Format takes it
	// from the caller rather than a clock so that images are reproducible.
	VolumeID uint32
	// HiddenSectors is the volume's first sector on the whole disk: the
	// partition's start, in logical sectors, or zero for a whole-device
	// volume. This filesystem ignores it, since it only ever sees its own
	// slice (ADR 0115), but Windows and firmware read it.
	HiddenSectors uint32
}

const (
	formatReserved   = 32
	formatInfoSector = 1
	formatBackupBoot = backupCandidate
	formatMedia      = 0xf8
)

// Format writes an empty FAT32 volume over the whole device, laid out as
// mkfs.fat and Windows lay one out: 32 reserved sectors holding the boot
// sector, FSInfo at sector 1 and their backups at sectors 6 and 7, then the
// FATs — sized for exactly the clusters that remain after them, their first
// entries holding the media byte and the root directory's end of chain — and
// a zeroed root directory cluster. The reserved region is zeroed first, so
// nothing a previous volume left there survives.
func Format(device Device, layout Layout) bool {
	sector := uint64(layout.BytesPerSector)
	if sector == 0 {
		sector = minSectorSize
	}
	perCluster := uint64(layout.SectorsPerCluster)
	copies := uint64(layout.FATs)
	if device == nil || sector < minSectorSize || sector > maxSectorSize || !powerOfTwo(sector) || !powerOfTwo(perCluster) ||
		copies == 0 || copies > maxFATs || layout.NoMirror && uint64(layout.ActiveFAT) >= copies {
		return false
	}
	capacity := device.Capacity()
	total := capacity / sector
	if total > uint64(^uint32(0)) {
		return false
	}
	fatSectors := uint64(1)
	clusters := uint64(0)
	for {
		metadata := formatReserved + copies*fatSectors
		if total <= metadata+perCluster {
			return false
		}
		clusters = (total - metadata) / perCluster
		needed := ((clusters+2)*4 + sector - 1) / sector
		if needed <= fatSectors {
			break
		}
		fatSectors = needed
	}
	if clusters > maxClusters {
		return false
	}
	var zero [zeroSize]byte
	if !zeroDevice(device, &zero, 0, formatReserved*sector) {
		return false
	}
	var boot [unit]byte
	boot[0], boot[1], boot[2] = 0xeb, 0x58, 0x90
	copy(boot[3:11], "GOOSFAT ")
	put16(boot[bpbBytes:], uint16(sector))
	boot[bpbPerCluster] = layout.SectorsPerCluster
	put16(boot[bpbReserved:], formatReserved)
	boot[bpbFATs] = layout.FATs
	boot[bpbMedia] = formatMedia
	put32(boot[bpbHidden:], layout.HiddenSectors)
	put32(boot[bpbTotal32:], uint32(total))
	put32(boot[bpbFATSize32:], uint32(fatSectors))
	if layout.NoMirror {
		put16(boot[bpbExtFlags:], extNoMirror|uint16(layout.ActiveFAT))
	}
	put32(boot[bpbRootCluster:], 2)
	put16(boot[bpbFSInfo:], formatInfoSector)
	put16(boot[bpbBackup:], formatBackupBoot)
	boot[64], boot[66] = 0x80, 0x29
	put32(boot[67:71], layout.VolumeID)
	copy(boot[71:82], "GOOS       ")
	copy(boot[82:90], "FAT32   ")
	boot[bpbSignature], boot[bpbSignature+1] = 0x55, 0xaa
	var info [unit]byte
	put32(info[0:], fsInfoLead)
	put32(info[fsInfoStructAt:], fsInfoStruct)
	put32(info[fsInfoFreeCount:], uint32(clusters-1))
	put32(info[fsInfoNextFree:], 3)
	put32(info[fsInfoTrailAt:], fsInfoTrail)
	for _, at := range [2]uint64{0, formatBackupBoot} {
		if !device.WriteAt(at*sector, boot[:]) || !device.WriteAt((at+formatInfoSector)*sector, info[:]) {
			return false
		}
	}
	fatBytes := fatSectors * sector
	for index := uint64(0); index < copies; index++ {
		start := (formatReserved + index*fatSectors) * sector
		if !zeroDevice(device, &zero, start, fatBytes) {
			return false
		}
		var head [12]byte
		put32(head[0:4], 0x0fffff00|formatMedia)
		put32(head[4:8], endOfChainMark)
		put32(head[8:12], endOfChainMark)
		if !device.WriteAt(start, head[:]) {
			return false
		}
	}
	root := (formatReserved + copies*fatSectors) * sector
	return zeroDevice(device, &zero, root, perCluster*sector) && device.Flush()
}

func zeroDevice(device Device, zero *[zeroSize]byte, offset, length uint64) bool {
	for length != 0 {
		chunk := min(uint64(len(zero)), length)
		if !device.WriteAt(offset, zero[:chunk]) {
			return false
		}
		offset += chunk
		length -= chunk
	}
	return true
}
