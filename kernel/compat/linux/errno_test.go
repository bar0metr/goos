package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/stream"
	"goos/kernel/vfs"
)

func TestVFSErrnoKeepsOperationContext(t *testing.T) {
	if got := VFSErrno(vfs.StatusPermissionDenied, VFSPermissionAccess, 0); got != abi.EACCES {
		t.Fatalf("access denial = %d", got)
	}
	if got := VFSErrno(vfs.StatusPermissionDenied, VFSPermissionReadOnly, 0); got != abi.EROFS {
		t.Fatalf("read-only denial = %d", got)
	}
	if got := VFSErrno(vfs.StatusResourceExhausted, VFSPermissionAccess, abi.EMFILE); got != abi.EMFILE {
		t.Fatalf("fd exhaustion = %d", got)
	}
	if got := VFSErrno(vfs.StatusResourceExhausted, VFSPermissionAccess, 0); got != abi.EIO {
		t.Fatalf("ambiguous exhaustion = %d", got)
	}
}

func TestVFSErrnoPreservesFilesystemReasons(t *testing.T) {
	cases := map[vfs.Status]abi.Errno{
		vfs.StatusNotFound: abi.ENOENT, vfs.StatusAlreadyExists: abi.EEXIST,
		vfs.StatusNotDirectory: abi.ENOTDIR, vfs.StatusIsDirectory: abi.EISDIR,
		vfs.StatusNotEmpty: abi.ENOTEMPTY, vfs.StatusNoSpace: abi.ENOSPC,
		vfs.StatusNameTooLong: abi.ENAMETOOLONG, vfs.StatusUnsupported: abi.EOPNOTSUPP,
	}
	for status, want := range cases {
		if got := VFSErrno(status, VFSPermissionAccess, 0); got != want {
			t.Fatalf("status %d = errno %d, want %d", status, got, want)
		}
	}
}

func TestStreamErrnoDependsOnDirection(t *testing.T) {
	if got := StreamReadErrno(stream.StatusPeerClosed); got != 0 {
		t.Fatalf("read EOF = %d", got)
	}
	if got := StreamWriteErrno(stream.StatusPeerClosed); got != abi.EPIPE {
		t.Fatalf("broken write = %d", got)
	}
}
