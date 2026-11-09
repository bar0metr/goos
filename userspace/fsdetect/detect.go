// Package fsdetect recognizes the filesystem on a block slice and names the
// driver that may serve it (ADR 0222). Recognition reads a bounded window at
// fixed offsets and checks signatures and basic geometry. It never decides
// that a volume is usable: the driver validates the volume before it serves
// anything.
package fsdetect

import "goos/kernel/ramfs"

// ProbeSize is the most a probe reads, from the start of the slice.
const ProbeSize = 64 << 10

// Window is the part of the slice a probe needs: the FAT boot sector at 0
// and the ext4 superblock at 1024.
const Window = 4096

// Status says what a probe found.
type Status uint8

const (
	// Recognized: exactly one known filesystem matches.
	Recognized Status = iota + 1
	// Unknown: no known filesystem matches.
	Unknown
	// Ambiguous: more than one filesystem matches, so none is chosen.
	Ambiguous
	// TooSmall: the slice cannot hold any known filesystem.
	TooSmall
)

func (status Status) String() string {
	switch status {
	case Recognized:
		return "recognized"
	case Unknown:
		return "unsupported filesystem"
	case Ambiguous:
		return "ambiguous filesystem signatures"
	case TooSmall:
		return "slice too small"
	}
	return "unknown status"
}

// MaxIdentity is the longest volume identity a probe reports.
const MaxIdentity = 16

// MaxLabel is the longest label a probe reports.
const MaxLabel = 16

// Result is a probe's finding.
type Result struct {
	Status Status
	Type   ramfs.FilesystemType
	// Identity distinguishes this volume from another of the same type: the
	// ext4 UUID or the FAT32 volume serial. A remount after a crash must
	// find the same identity.
	Identity     [MaxIdentity]byte
	IdentitySize uint8
	Label        [MaxLabel]byte
	LabelSize    uint8
}

// SameVolume reports whether two probes found the same volume.
func (result *Result) SameVolume(other *Result) bool {
	if result == nil || other == nil || result.Status != Recognized || other.Status != Recognized ||
		result.Type != other.Type || result.IdentitySize == 0 || result.IdentitySize > MaxIdentity ||
		result.IdentitySize != other.IdentitySize || result.Identity != other.Identity {
		return false
	}
	for _, value := range result.Identity[:result.IdentitySize] {
		if value != 0 {
			return true
		}
	}
	return false
}

// Probe recognizes the filesystem whose first bytes are window, on a slice
// of size bytes. window holds at least Window bytes, or the whole slice when
// it is smaller.
func Probe(window []byte, size uint64) Result {
	if size < Window || uint64(len(window)) < Window {
		return Result{Status: TooSmall}
	}
	var found Result
	matches := 0
	if result, ok := probeExt4(window, size); ok {
		found = result
		matches++
	}
	if result, ok := probeFAT32(window, size); ok {
		found = result
		matches++
	}
	switch matches {
	case 0:
		return Result{Status: Unknown}
	case 1:
		found.Status = Recognized
		return found
	}
	return Result{Status: Ambiguous}
}

func le16(data []byte, offset int) uint16 { return uint16(data[offset]) | uint16(data[offset+1])<<8 }

func le32(data []byte, offset int) uint32 {
	return uint32(le16(data, offset)) | uint32(le16(data, offset+2))<<16
}

// probeExt4 checks the superblock at byte 1024: magic, a revision Linux
// writes, a block size of 1 to 64 KiB, nonzero groups, and a block count
// that fits the slice. ext2 and ext3 volumes match as well; the ext4 driver
// decides which of their features it accepts.
func probeExt4(window []byte, size uint64) (Result, bool) {
	sb := window[1024:2048]
	if le16(sb, 0x38) != 0xEF53 || le32(sb, 0x4C) > 1 || le32(sb, 0x18) > 6 {
		return Result{}, false
	}
	blockSize := uint64(1024) << le32(sb, 0x18)
	blocks := uint64(le32(sb, 0x4))
	if le32(sb, 0x60)&0x80 != 0 {
		blocks |= uint64(le32(sb, 0x150)) << 32
	}
	if le32(sb, 0x0) == 0 || le32(sb, 0x20) == 0 || le32(sb, 0x28) == 0 || blocks == 0 || blocks > size/blockSize {
		return Result{}, false
	}
	result := Result{Type: ramfs.FilesystemExt4, IdentitySize: 16}
	copy(result.Identity[:], sb[0x68:0x78])
	result.LabelSize = uint8(copy(result.Label[:], trimLabel(sb[0x78:0x88], 0)))
	return result, true
}

// probeFAT32 checks the boot sector: the 0x55AA signature, a sector size and
// cluster size the format allows, the FAT32 shape (no root entry count, no
// 16-bit sizes, a 32-bit FAT size and root cluster), and a total size that
// fits the slice.
func probeFAT32(window []byte, size uint64) (Result, bool) {
	boot := window[:512]
	if boot[510] != 0x55 || boot[511] != 0xAA {
		return Result{}, false
	}
	sector := uint64(le16(boot, 0x0B))
	perCluster := boot[0x0D]
	if sector != 512 && sector != 1024 && sector != 2048 && sector != 4096 {
		return Result{}, false
	}
	if perCluster == 0 || perCluster&(perCluster-1) != 0 || le16(boot, 0x0E) == 0 || (boot[0x10] != 1 && boot[0x10] != 2) {
		return Result{}, false
	}
	if le16(boot, 0x11) != 0 || le16(boot, 0x13) != 0 || le16(boot, 0x16) != 0 || le32(boot, 0x24) == 0 || le32(boot, 0x2C) < 2 {
		return Result{}, false
	}
	total := uint64(le32(boot, 0x20))
	if total == 0 || total > size/sector {
		return Result{}, false
	}
	result := Result{Type: ramfs.FilesystemFAT32}
	if boot[0x42] == 0x29 {
		result.IdentitySize = 4
		copy(result.Identity[:], boot[0x43:0x47])
		result.LabelSize = uint8(copy(result.Label[:], trimLabel(boot[0x47:0x52], ' ')))
	}
	return result, true
}

// trimLabel drops trailing padding: NUL for ext4, spaces for FAT.
func trimLabel(label []byte, pad byte) []byte {
	end := len(label)
	for end > 0 && (label[end-1] == pad || label[end-1] == 0) {
		end--
	}
	if pad == 0 {
		for index, value := range label[:end] {
			if value == 0 {
				return label[:index]
			}
		}
	}
	return label[:end]
}
