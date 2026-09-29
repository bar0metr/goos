package linux

import "goos/kernel/compat/linux/abi"

const (
	LinuxPageSize = uint64(4096)
	BrkReserve    = uint64(64 << 20)
)

func BrkBounds(imageEnd uint64, pageSize uint64, upper uint64) (uint64, uint64, bool) {
	if pageSize == 0 || pageSize&(pageSize-1) != 0 || imageEnd == 0 || imageEnd > ^uint64(0)-(pageSize-1) {
		return 0, 0, false
	}
	base := (imageEnd + pageSize - 1) &^ (pageSize - 1)
	if base == 0 || base >= upper || BrkReserve > upper-base {
		return 0, 0, false
	}
	return base, base + BrkReserve, true
}

func PageRange(address uint64, length uint64, pageSize uint64) (uint64, abi.Errno) {
	if pageSize == 0 || pageSize&(pageSize-1) != 0 || address%pageSize != 0 || length == 0 || length > ^uint64(0)-(pageSize-1) {
		return 0, abi.EINVAL
	}
	rounded := (length + pageSize - 1) &^ (pageSize - 1)
	if rounded == 0 || address > ^uint64(0)-rounded {
		return 0, abi.EINVAL
	}
	return rounded, 0
}

func AnonymousMapping(arguments [6]uint64, pageSize uint64) (uint64, uint64, abi.Errno) {
	address, length, protection := arguments[0], arguments[1], arguments[2]
	flags, fd, offset := arguments[3], arguments[4], arguments[5]
	if address != 0 || flags != abi.MapPrivate|abi.MapAnonymous || fd != ^uint64(0) || offset != 0 {
		return 0, 0, abi.EINVAL
	}
	if _, errno := MemoryProtection(protection); errno != 0 {
		return 0, 0, errno
	}
	rounded, errno := roundLength(length, pageSize)
	if errno != 0 {
		return 0, 0, errno
	}
	return rounded, protection, 0
}

func roundLength(length uint64, pageSize uint64) (uint64, abi.Errno) {
	if pageSize == 0 || pageSize&(pageSize-1) != 0 || length == 0 || length > ^uint64(0)-(pageSize-1) {
		return 0, abi.EINVAL
	}
	rounded := (length + pageSize - 1) &^ (pageSize - 1)
	if rounded == 0 {
		return 0, abi.EINVAL
	}
	return rounded, 0
}

func MemoryProtection(protection uint64) (uint8, abi.Errno) {
	if protection != abi.ProtNone && protection != abi.ProtRead && protection != abi.ProtRead|abi.ProtWrite {
		return 0, abi.EINVAL
	}
	return uint8(protection), 0
}

func UserFSBase(address uint64, userLimit uint64) bool {
	return userLimit != 0 && address < userLimit
}
