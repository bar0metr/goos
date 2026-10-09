//go:build goos_userspace

package tlsclient

import (
	"errors"
	"goos/internal/validation"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/tcpapi"
	"goos/userspace/utility"
)

// LoadSystemRoots reads only the immutable system trust-store path.
func LoadSystemRoots(bulk *utility.Bulk) (*Roots, error) {
	return loadNativeRoots(RootPath, bulk)
}

// LoadProductionProbeRoots measures the committed bundle in a validation image.
// Fetch does not call this path or use it as a fallback.
func LoadProductionProbeRoots(bulk *utility.Bulk) (*Roots, error) {
	if !validation.Enabled {
		return nil, errors.New("production bundle probe requires a validation image")
	}
	return loadNativeRoots("/system/security/probe-production.pem", bulk)
}

func loadNativeRoots(name string, bulk *utility.Bulk) (*Roots, error) {
	path := []byte(name)
	kind, size, status := utility.Stat(path)
	if status != vfs.StatusOK || kind != ramfs.KindFile || size == 0 || size > MaxBundle {
		return nil, errors.New("system trust store unavailable or oversized")
	}
	if len(bulk.Memory) == 0 {
		var ok bool
		*bulk, ok = utility.NewBulk(tcpapi.SlotSize)
		if !ok {
			return nil, errors.New("trust store buffer unavailable")
		}
	}
	reply, status := utility.Request(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if status != vfs.StatusOK {
		return nil, errors.New("system trust store unavailable")
	}
	file := reply.Handle
	data := make([]byte, int(size))
	var readErr error
	for offset := 0; offset < len(data); {
		count := len(data) - offset
		if count > len(bulk.Memory) {
			count = len(bulk.Memory)
		}
		reply, received := bulk.ReadFile(file, count)
		if !received || reply.Status != vfs.StatusOK || reply.Size == 0 || reply.Size > uint64(count) {
			readErr = errors.New("system trust store read failed")
			break
		}
		n := int(reply.Size)
		copy(data[offset:offset+n], bulk.Memory[:n])
		offset += n
	}
	_, closeStatus := utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: file})
	if readErr != nil {
		return nil, readErr
	}
	if closeStatus != vfs.StatusOK {
		return nil, errors.New("system trust store close failed")
	}
	return LoadRoots(data)
}
