package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/kernel/stream"
	"goos/kernel/vfs"
)

// VFSPermissionContext preserves why a native permission denial occurred.
// The VFS status alone deliberately does not guess EACCES versus EROFS.
type VFSPermissionContext uint8

const (
	VFSPermissionAccess VFSPermissionContext = iota
	VFSPermissionReadOnly
)

// VFSErrno translates one VFS result. exhaustion names the operation-specific
// resource that filled; zero is rejected because ResourceExhausted has no
// context-free Linux meaning.
func VFSErrno(status vfs.Status, permission VFSPermissionContext, exhaustion abi.Errno) abi.Errno {
	switch status {
	case vfs.StatusOK:
		return 0
	case vfs.StatusInvalidArgument:
		return abi.EINVAL
	case vfs.StatusNotFound:
		return abi.ENOENT
	case vfs.StatusAlreadyExists:
		return abi.EEXIST
	case vfs.StatusPermissionDenied:
		if permission == VFSPermissionReadOnly {
			return abi.EROFS
		}
		return abi.EACCES
	case vfs.StatusResourceExhausted:
		if exhaustion == abi.EMFILE || exhaustion == abi.ENFILE || exhaustion == abi.ENOMEM || exhaustion == abi.ENOSPC {
			return exhaustion
		}
		return abi.EIO
	case vfs.StatusBackendError, vfs.StatusPeerDied:
		return abi.EIO
	case vfs.StatusNotDirectory:
		return abi.ENOTDIR
	case vfs.StatusIsDirectory:
		return abi.EISDIR
	case vfs.StatusNotEmpty:
		return abi.ENOTEMPTY
	case vfs.StatusBusy:
		return abi.EBUSY
	case vfs.StatusNoSpace:
		return abi.ENOSPC
	case vfs.StatusCorrupt:
		return abi.EUCLEAN
	case vfs.StatusNameTooLong:
		return abi.ENAMETOOLONG
	case vfs.StatusTimeout:
		return abi.ETIMEDOUT
	case vfs.StatusUnsupported:
		return abi.EOPNOTSUPP
	default:
		return abi.EIO
	}
}

func StreamReadErrno(status stream.Status) abi.Errno {
	switch status {
	case stream.StatusOK, stream.StatusPeerClosed:
		return 0
	case stream.StatusWouldBlock:
		return abi.EAGAIN
	default:
		return abi.EBADF
	}
}

func StreamWriteErrno(status stream.Status) abi.Errno {
	switch status {
	case stream.StatusOK:
		return 0
	case stream.StatusWouldBlock:
		return abi.EAGAIN
	case stream.StatusPeerClosed:
		return abi.EPIPE
	default:
		return abi.EBADF
	}
}
