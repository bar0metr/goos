//go:build goos_userspace

// Package nativefiles provides bounded, closed-handle VFS file operations.
package nativefiles

import (
	"errors"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/runtime/arena"
	"goos/userspace/truststate"
	"goos/userspace/utility"
	"strconv"
)

type Files struct{ Bulk *utility.Bulk }

func statusError(status vfs.Status) error {
	if status == vfs.StatusNotFound {
		return truststate.ErrNotFound
	}
	return errors.New("trust VFS operation failed: " + strconv.Itoa(int(status)))
}

func (f *Files) buffer() error {
	if f.Bulk == nil {
		return errors.New("trust file buffer unavailable")
	}
	if len(f.Bulk.Memory) == 0 {
		var ok bool
		*f.Bulk, ok = utility.NewBulk(utility.BulkSize)
		if !ok {
			return errors.New("trust file buffer allocation failed")
		}
	}
	return nil
}

func (f *Files) Read(path string, limit int) ([]byte, error) {
	kind, size, status := utility.Stat([]byte(path))
	if status != vfs.StatusOK {
		return nil, statusError(status)
	}
	if kind != ramfs.KindFile || size > uint64(limit) {
		return nil, errors.New("trust file kind or size outside bounds")
	}
	if err := f.buffer(); err != nil {
		return nil, err
	}
	reply, status := utility.Request(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte(path)})
	if status != vfs.StatusOK {
		return nil, statusError(status)
	}
	file := reply.Handle
	data := make([]byte, int(size))
	var err error
	for offset := 0; offset < len(data); {
		count := len(data) - offset
		if count > len(f.Bulk.Memory) {
			count = len(f.Bulk.Memory)
		}
		mark := arena.Current()
		reply, received := f.Bulk.ReadFile(file, count)
		arena.Reset(mark)
		if !received || reply.Status != vfs.StatusOK || reply.Size == 0 || reply.Size > uint64(count) {
			err = errors.New("trust file read failed")
			break
		}
		n := int(reply.Size)
		copy(data[offset:offset+n], f.Bulk.Memory[:n])
		offset += n
	}
	if err == nil {
		stat, checked := utility.Request(vfs.Request{Operation: vfs.OperationStat, Handle: file})
		if checked != vfs.StatusOK || stat.Size != size {
			err = errors.New("trust file changed during read")
		}
	}
	_, status = utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: file})
	if err != nil {
		return nil, err
	}
	if status != vfs.StatusOK {
		return nil, statusError(status)
	}
	return data, nil
}

func (f *Files) Create(path string, data []byte) error {
	if err := f.buffer(); err != nil {
		return err
	}
	if status := utility.CreateFile([]byte(path)); status != vfs.StatusOK {
		return statusError(status)
	}
	reply, status := utility.Request(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte(path)})
	if status != vfs.StatusOK {
		return statusError(status)
	}
	file := reply.Handle
	var err error
	for len(data) > 0 {
		n := len(data)
		if n > len(f.Bulk.Memory) {
			n = len(f.Bulk.Memory)
		}
		copy(f.Bulk.Memory, data[:n])
		mark := arena.Current()
		reply, received := f.Bulk.WriteFile(file, n)
		arena.Reset(mark)
		if !received || reply.Status != vfs.StatusOK || reply.Size != uint64(n) {
			err = errors.New("trust file write failed")
			break
		}
		data = data[n:]
	}
	_, status = utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: file})
	if err != nil {
		return err
	}
	if status != vfs.StatusOK {
		return statusError(status)
	}
	return nil
}

func (*Files) Mkdir(path string) error {
	status := utility.Mkdir([]byte(path))
	if status == vfs.StatusAlreadyExists {
		kind, _, checked := utility.Stat([]byte(path))
		if checked == vfs.StatusOK && kind == ramfs.KindDirectory {
			return nil
		}
	}
	if status != vfs.StatusOK {
		return statusError(status)
	}
	return nil
}
func (*Files) Rename(source, target string) error {
	if status := utility.Rename([]byte(source), []byte(target)); status != vfs.StatusOK {
		return statusError(status)
	}
	return nil
}
func (*Files) Replace(source, target string) error {
	if status := utility.Replace([]byte(source), []byte(target)); status != vfs.StatusOK {
		return statusError(status)
	}
	return nil
}

func (*Files) Remove(path string) error {
	if status := utility.Unlink([]byte(path)); status != vfs.StatusOK {
		return statusError(status)
	}
	return nil
}

type listing struct {
	names    []string
	limit    int
	exceeded bool
}

func (l *listing) Visit(name []byte, kind ramfs.Kind, _ uint64, _ ramfs.Timestamp) bool {
	if kind != ramfs.KindDirectory {
		return true
	}
	if len(l.names) == l.limit {
		l.exceeded = true
		return false
	}
	l.names = append(l.names, string(name))
	return true
}
func (*Files) List(path string, limit int) ([]string, error) {
	result := listing{limit: limit}
	status := utility.ReadDir([]byte(path), &result)
	if status != vfs.StatusOK {
		return nil, statusError(status)
	}
	if result.exceeded {
		return nil, errors.New("too many trust generations")
	}
	return result.names, nil
}

// The installer lock lives on RAMFS, never on the persistent state volume.
// A crashed command can leave it until reboot, without stranding active roots.
func (f *Files) Lock() error {
	info, _, status := utility.FilesystemInfo([]byte("/run"))
	if status != vfs.StatusOK || info.Type != ramfs.FilesystemRAMFS || info.ReadOnly {
		return errors.New("volatile trust install lock unavailable")
	}
	if err := f.Create("/run/trust-update.lock", []byte("GOOS-TRUST-INSTALL-V1\n")); err != nil {
		return errors.New("trust installer lock unavailable or busy; reboot clears an interrupted installer")
	}
	return nil
}
func (*Files) Unlock() error {
	if status := utility.Unlink([]byte("/run/trust-update.lock")); status != vfs.StatusOK {
		return statusError(status)
	}
	return nil
}
