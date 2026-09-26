package fat32

import "goos/kernel/ramfs"

// The boot sector and its BIOS parameter block (BPB). Every field is untrusted:
// parseBoot derives the volume's geometry only after each field, and every
// product of fields, has been checked against the others and the device.

const (
	minSectorSize = 512
	// maxSectorSize is the largest logical sector this implementation reads.
	// FAT32 allows 512, 1024, 2048 and 4096; a larger power of two is a
	// volume this service does not implement rather than a corrupt one.
	maxSectorSize = 4096
	// backupCandidate is the logical sector every formatter puts the backup
	// boot sector at, and where it is looked for when the primary is unusable.
	backupCandidate = 6
	// noSector is how the BPB says "none" for the FSInfo and backup sectors,
	// besides zero.
	noSector = 0xffff
)

// Offsets of the BPB fields read here, in the boot sector.
const (
	bpbOEM          = 3
	bpbBytes        = 11
	bpbPerCluster   = 13
	bpbReserved     = 14
	bpbFATs         = 16
	bpbRootEntries  = 17
	bpbTotal16      = 19
	bpbMedia        = 21
	bpbFATSize16    = 22
	bpbTotal32      = 32
	bpbFATSize32    = 36
	bpbExtFlags     = 40
	bpbVersion      = 42
	bpbRootCluster  = 44
	bpbFSInfo       = 48
	bpbBackup       = 50
	bpbSignature    = 510
	bpbCompareStart = bpbBytes
	// bpbCompareEnd ends the fields a backup boot sector must repeat: the BPB
	// and the extended BPB up to the file-system type. Boot code and the OEM
	// name may legitimately differ between the two copies.
	bpbCompareEnd = 90
)

// BootSource says which boot sector a mount took the volume's geometry from.
type BootSource uint8

const (
	BootPrimary BootSource = iota
	// BootBackup: the primary boot sector failed validation and the backup
	// at logical sector 6 was used instead. The volume is mounted read-only
	// and nothing is rewritten; restoring the primary is a repair.
	BootBackup
)

// BackupState is what a mount found at the backup boot sector.
type BackupState uint8

const (
	// BackupAbsent: the BPB names no backup boot sector.
	BackupAbsent BackupState = iota
	// BackupMatches: the backup repeats the BPB of the sector mounted from.
	BackupMatches
	// BackupDiffers: the backup is a valid boot sector with a different BPB.
	// The primary is authoritative, as in every other implementation.
	BackupDiffers
	// BackupInvalid: the BPB names a backup that is unreadable, lies outside
	// the reserved region, or does not validate.
	BackupInvalid
)

// FSInfoState is what a mount found at the FSInfo sector.
type FSInfoState uint8

const (
	FSInfoAbsent FSInfoState = iota
	// FSInfoValid: the three signatures match; its hints are used after range
	// checks and the sector is kept up to date.
	FSInfoValid
	// FSInfoInvalid: the BPB names a sector that is outside the reserved
	// region or fails its signatures. It is neither read nor written.
	FSInfoInvalid
)

// BackupFSInfoState is the mount-time state of the backup FSInfo sector.
type BackupFSInfoState uint8

const (
	BackupFSInfoAbsent BackupFSInfoState = iota
	BackupFSInfoMatches
	BackupFSInfoDiffers
	BackupFSInfoInvalid
)

// Health is what a mount noticed about a volume it nevertheless accepted.
// Nothing here is repaired by mounting; it is reported so that the service can
// log it and a checker can act on it.
type Health struct {
	Boot         BootSource
	Backup       BackupState
	FSInfo       FSInfoState
	BackupFSInfo BackupFSInfoState
	// ReadOnly: the mount refuses every change with permission denied,
	// because the geometry came from backup metadata.
	ReadOnly bool
	// ShortFAT: the FAT describes fewer clusters than the data region holds;
	// the clusters beyond it are never used, as in Linux.
	ShortFAT bool
	// FewClusters: the volume has the FAT32 layout but fewer than 65525
	// clusters, which some systems would read as FAT16. It is served as the
	// FAT32 its BPB declares, as Linux does.
	FewClusters bool
}

// Degraded reports whether the volume was accepted with a problem worth
// reporting.
func (health Health) Degraded() bool {
	return health.Boot != BootPrimary || health.Backup == BackupDiffers || health.Backup == BackupInvalid ||
		health.FSInfo == FSInfoInvalid || health.BackupFSInfo == BackupFSInfoDiffers ||
		health.BackupFSInfo == BackupFSInfoInvalid || health.ReadOnly
}

// fat16Clusters is the cluster count from which the FAT specification calls
// a volume FAT32.
const fat16Clusters = 65525

// geometry is a validated BPB. Sizes are in bytes unless the name says
// sectors; fsInfo and backup are logical sector numbers, zero when absent.
type geometry struct {
	sectorSize      uint64
	perCluster      uint64
	reserved        uint64
	fatSectors      uint64
	copies          uint32
	total           uint64
	clusters        uint64
	root            uint32
	flags           uint16
	fsInfo          uint64
	backup          uint64
	backupFSInfo    uint64
	badFSInfo       bool
	badBackupFSInfo bool
	badBackup       bool
	shortFAT        bool
	fewClusters     bool
}

func powerOfTwo(value uint64) bool { return value != 0 && value&(value-1) == 0 }

// validMedia is the media descriptor rule Linux applies: 0xf0 or 0xf8 and up.
func validMedia(media byte) bool { return media == 0xf0 || media >= 0xf8 }

// parseBoot validates the boot sector at the start of boot (at least the
// first 512 bytes of it) against a device of capacity bytes. It answers
// Unsupported for a volume that is not FAT32 or uses a feature this
// implementation does not (FAT12, FAT16, exFAT, NTFS, sectors over 4 KiB,
// more than two FATs, a newer FAT32 version) and Corrupt for one whose fields
// contradict each other or the device. Every product is formed in 64 bits
// from fields of at most 32 bits and a sector size of at most 4 KiB, so none
// can overflow.
func parseBoot(boot []byte, capacity uint64) (geometry, ramfs.Status) {
	var geo geometry
	if boot[bpbSignature] != 0x55 || boot[bpbSignature+1] != 0xaa {
		return geo, ramfs.StatusCorrupt
	}
	if oem := string(boot[bpbOEM : bpbOEM+8]); oem == "EXFAT   " || oem == "NTFS    " {
		return geo, ramfs.StatusUnsupported
	}
	geo.sectorSize = uint64(get16(boot[bpbBytes:]))
	switch {
	case geo.sectorSize < minSectorSize || !powerOfTwo(geo.sectorSize):
		return geo, ramfs.StatusCorrupt
	case geo.sectorSize > maxSectorSize:
		return geo, ramfs.StatusUnsupported
	}
	geo.perCluster = uint64(boot[bpbPerCluster])
	geo.reserved = uint64(get16(boot[bpbReserved:]))
	geo.copies = uint32(boot[bpbFATs])
	if !powerOfTwo(geo.perCluster) || geo.reserved == 0 || geo.copies == 0 {
		return geo, ramfs.StatusCorrupt
	}
	// A FAT32 BPB has no fixed root directory and no 16-bit FAT size; a
	// volume that declares either is FAT12 or FAT16, which this service does
	// not implement. The type follows the BPB rather than the cluster count,
	// as in Linux.
	if get16(boot[bpbRootEntries:]) != 0 || get16(boot[bpbFATSize16:]) != 0 {
		return geo, ramfs.StatusUnsupported
	}
	if geo.copies > maxFATs || get16(boot[bpbVersion:]) != 0 {
		return geo, ramfs.StatusUnsupported
	}
	if !validMedia(boot[bpbMedia]) {
		return geo, ramfs.StatusCorrupt
	}
	geo.fatSectors = uint64(get32(boot[bpbFATSize32:]))
	geo.total = uint64(get16(boot[bpbTotal16:]))
	if geo.total == 0 {
		geo.total = uint64(get32(boot[bpbTotal32:]))
	}
	if geo.fatSectors == 0 || geo.total == 0 || geo.total > capacity/geo.sectorSize {
		return geo, ramfs.StatusCorrupt
	}
	metadata := geo.reserved + uint64(geo.copies)*geo.fatSectors
	if geo.total <= metadata {
		return geo, ramfs.StatusCorrupt
	}
	geo.clusters = (geo.total - metadata) / geo.perCluster
	// Like Linux, a FAT too small for the data region limits the volume to
	// the clusters it can describe instead of refusing it: those beyond are
	// never allocated, so nothing reads an entry past the FAT.
	if described := geo.fatSectors*geo.sectorSize/4 - 2; geo.clusters > described {
		geo.clusters, geo.shortFAT = described, true
	}
	if geo.clusters == 0 || geo.clusters > maxClusters {
		return geo, ramfs.StatusCorrupt
	}
	geo.fewClusters = geo.clusters < fat16Clusters
	geo.flags = get16(boot[bpbExtFlags:])
	if geo.flags&extNoMirror != 0 && uint32(geo.flags&extActiveMask) >= geo.copies {
		return geo, ramfs.StatusCorrupt
	}
	geo.root = get32(boot[bpbRootCluster:])
	if geo.root < 2 || uint64(geo.root-2) >= geo.clusters {
		return geo, ramfs.StatusCorrupt
	}
	// The FSInfo and backup sectors must lie in the reserved region, apart
	// from the boot sector and from each other. One that does not is treated
	// as absent and reported, never followed.
	geo.fsInfo, geo.badFSInfo = reservedSector(get16(boot[bpbFSInfo:]), geo.reserved)
	geo.backup, geo.badBackup = reservedSector(get16(boot[bpbBackup:]), geo.reserved)
	if geo.fsInfo != 0 && geo.fsInfo == geo.backup {
		geo.fsInfo, geo.badFSInfo = 0, true
	}
	if geo.fsInfo != 0 && geo.backup != 0 {
		geo.backupFSInfo = geo.backup + geo.fsInfo
		if geo.backupFSInfo >= geo.reserved || geo.backupFSInfo == geo.backup {
			geo.backupFSInfo, geo.badBackupFSInfo = 0, true
		}
	} else if geo.fsInfo != 0 && geo.badBackup {
		geo.badBackupFSInfo = true
	}
	return geo, ramfs.StatusOK
}

// reservedSector checks a sector number the BPB names inside the reserved
// region. Zero and 0xffff mean none.
func reservedSector(value uint16, reserved uint64) (uint64, bool) {
	if value == 0 || value == noSector {
		return 0, false
	}
	if uint64(value) >= reserved {
		return 0, true
	}
	return uint64(value), false
}

// sameBPB reports whether two boot sectors carry the same BPB.
func sameBPB(first, second []byte) bool {
	for index := bpbCompareStart; index < bpbCompareEnd; index++ {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

// checkBackup compares the backup boot sector the BPB names with the primary.
func checkBackup(device Device, primary []byte, geo *geometry, capacity uint64) BackupState {
	switch {
	case geo.badBackup:
		return BackupInvalid
	case geo.backup == 0:
		return BackupAbsent
	}
	var backup [unit]byte
	if !device.ReadAt(geo.backup*geo.sectorSize, backup[:]) {
		return BackupInvalid
	}
	if _, status := parseBoot(backup[:], capacity); status != ramfs.StatusOK {
		return BackupInvalid
	}
	if !sameBPB(primary, backup[:]) {
		return BackupDiffers
	}
	return BackupMatches
}

// recoverBoot looks for a usable backup boot sector when the primary is
// corrupt. The primary's own sector size cannot be believed, so the backup is
// looked for at logical sector 6 for every supported sector size, and one is
// accepted only if it validates, declares that sector size, and names itself
// as the backup.
func recoverBoot(device Device, capacity uint64, boot []byte) (geometry, bool) {
	for size := uint64(minSectorSize); size <= maxSectorSize; size *= 2 {
		if backupCandidate*size+unit > capacity || !device.ReadAt(backupCandidate*size, boot[:unit]) {
			continue
		}
		geo, status := parseBoot(boot, capacity)
		if status == ramfs.StatusOK && geo.sectorSize == size && geo.backup == backupCandidate {
			return geo, true
		}
	}
	return geometry{}, false
}
