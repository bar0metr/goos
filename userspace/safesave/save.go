// Package safesave writes a file's new contents beside it and replaces the
// file only once they are complete (ADR 0129). No step truncates or rewrites
// the original: until the replacement succeeds, it holds what it held before.
package safesave

import (
	"goos/internal/limits"
	"goos/kernel/vfs"
)

// Suffix ends the name of the temporary file a save writes. The name also
// starts with a dot, so listings that hide such names hide it too.
const Suffix = ".~save"

// Stage says which step of a save failed.
type Stage uint8

const (
	// StageTemporary is creating or opening the temporary file.
	StageTemporary Stage = iota + 1
	// StageWrite is writing the new contents to it.
	StageWrite
	// StageReplace is replacing the target with it.
	StageReplace
)

// Files is the part of the VFS a save uses.
type Files interface {
	Create(path []byte) vfs.Status
	Unlink(path []byte) vfs.Status
	OpenWrite(path []byte) (vfs.Handle, vfs.Status)
	// Write writes data, which is always a prefix of the buffer Save was
	// given, and returns how much of it was written.
	Write(file vfs.Handle, data []byte) (int, vfs.Status)
	Close(file vfs.Handle) vfs.Status
	Replace(source, target []byte) vfs.Status
}

// Source produces the new contents. Fill writes the next bytes into buffer and
// returns how many; zero ends the contents.
type Source interface {
	Fill(buffer []byte) int
}

// TemporaryPath writes into out the path a save of target writes first:
// "." + name + Suffix in target's own directory, so the replacement stays
// within one filesystem and the name cannot leave the directory.
func TemporaryPath(target []byte, out *[limits.PathMax]byte) ([]byte, vfs.Status) {
	if len(target) < 2 || target[0] != '/' || target[len(target)-1] == '/' {
		return nil, vfs.StatusInvalidArgument
	}
	slash := len(target) - 1
	for target[slash] != '/' {
		slash--
	}
	name := target[slash+1:]
	if len(name) == 1 && name[0] == '.' || len(name) == 2 && name[0] == '.' && name[1] == '.' {
		return nil, vfs.StatusInvalidArgument
	}
	for _, value := range name {
		if value == 0 {
			return nil, vfs.StatusInvalidArgument
		}
	}
	if 1+len(name)+len(Suffix) > limits.NameMax || len(target)+1+len(Suffix) > limits.PathMax {
		return nil, vfs.StatusNameTooLong
	}
	size := copy(out[:], target[:slash+1])
	out[size] = '.'
	size++
	size += copy(out[size:], name)
	size += copy(out[size:], Suffix)
	return out[:size], vfs.StatusOK
}

// Save writes what source produces to the temporary file beside target,
// through buffer, and then replaces target with it. A temporary file left by
// an earlier, interrupted save is removed first. On any failure the temporary
// file is removed, best effort, and target is as it was; the status says why
// and the stage where.
func Save(files Files, target []byte, buffer []byte, source Source, temp *[limits.PathMax]byte) (vfs.Status, Stage) {
	if len(buffer) == 0 {
		return vfs.StatusInvalidArgument, StageWrite
	}
	path, status := TemporaryPath(target, temp)
	if status != vfs.StatusOK {
		return status, StageTemporary
	}
	status = files.Create(path)
	if status == vfs.StatusAlreadyExists {
		if status = files.Unlink(path); status == vfs.StatusOK {
			status = files.Create(path)
		}
	}
	if status != vfs.StatusOK {
		return status, StageTemporary
	}
	file, status := files.OpenWrite(path)
	if status != vfs.StatusOK {
		files.Unlink(path)
		return status, StageTemporary
	}
	status = write(files, file, buffer, source)
	if closed := files.Close(file); status == vfs.StatusOK {
		status = closed
	}
	if status != vfs.StatusOK {
		files.Unlink(path)
		return status, StageWrite
	}
	if status = files.Replace(path, target); status != vfs.StatusOK {
		files.Unlink(path)
		return status, StageReplace
	}
	return vfs.StatusOK, 0
}

func write(files Files, file vfs.Handle, buffer []byte, source Source) vfs.Status {
	for {
		count := source.Fill(buffer)
		if count <= 0 {
			return vfs.StatusOK
		}
		if count > len(buffer) {
			return vfs.StatusInvalidArgument
		}
		written, status := files.Write(file, buffer[:count])
		if status != vfs.StatusOK {
			return status
		}
		if written != count {
			// A filesystem that takes less than it was given is full.
			return vfs.StatusNoSpace
		}
	}
}
