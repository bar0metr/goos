// Package fsdisplay formats native filesystem information for small commands.
package fsdisplay

import "goos/kernel/ramfs"

// TypeName returns the stable user-facing name of a native filesystem type.
func TypeName(value ramfs.FilesystemType) string {
	switch value {
	case ramfs.FilesystemRAMFS:
		return "ramfs"
	case ramfs.FilesystemSystemImage:
		return "image"
	case ramfs.FilesystemFAT32:
		return "fat32"
	default:
		return "unknown"
	}
}

// Flag returns the mutation state enforced by the filesystem.
func Flag(readOnly bool) string {
	if readOnly {
		return "ro"
	}
	return "rw"
}

// Used returns allocated bytes without unsigned underflow.
func Used(info ramfs.FilesystemInfo) (uint64, bool) {
	if !info.CapacityAvailable || info.FreeBytes > info.TotalBytes {
		return 0, false
	}
	return info.TotalBytes - info.FreeBytes, true
}

// AppendSize appends a deterministic binary size with one decimal and no
// floating point. Unavailable capacity is a single dash.
func AppendSize(target []byte, size uint64, available bool) []byte {
	if !available {
		return append(target, '-')
	}
	units := [...]struct {
		divisor uint64
		suffix  byte
	}{{1 << 50, 'P'}, {1 << 40, 'T'}, {1 << 30, 'G'}, {1 << 20, 'M'}, {1 << 10, 'K'}}
	for _, unit := range units {
		if size < unit.divisor {
			continue
		}
		whole := size / unit.divisor
		tenth := (size%unit.divisor*10 + unit.divisor/2) / unit.divisor
		if tenth == 10 {
			whole, tenth = whole+1, 0
		}
		target = appendUint(target, whole)
		return append(target, '.', byte(tenth)+'0', unit.suffix)
	}
	return append(appendUint(target, size), 'B')
}

func appendUint(target []byte, value uint64) []byte {
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte(value%10) + '0'
		value /= 10
		if value == 0 {
			break
		}
	}
	return append(target, digits[position:]...)
}
