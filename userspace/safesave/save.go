// Package safesave writes a file's new contents beside it and replaces the
// file only once they are complete (ADR 0129). No step truncates or rewrites
// the original: until the replacement succeeds, it holds what it held before.
package safesave

import (
	"goos/internal/limits"
	"goos/kernel/vfs"
	"sync/atomic"
)

// Suffix ends the deterministic temporary-file prefix. Save appends a candidate
// ID to this prefix; names start with a dot so ordinary listings hide them.
const Suffix = ".~save"

const (
	temporaryAttempts = 32
	temporaryIDBytes  = 1 + 16 // dot followed by a fixed-width hexadecimal uint64
)

// temporarySequence never wraps or reuses an ID within this process. It only
// proposes names: exclusive Create, not the counter, establishes ownership
// across processes. Existing candidates are skipped without being changed.
var temporarySequence atomic.Uint64

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
	// Create atomically creates a file only if path is absent. An existing
	// file is left unchanged and reports StatusAlreadyExists.
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

// TemporaryPath writes the deterministic prefix for Save's candidate names:
// "." + name + Suffix in target's own directory. Save adds its candidate ID
// and checks the resulting path and component lengths before creating a file.
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

func nextTemporaryID(sequence *atomic.Uint64) (uint64, bool) {
	for attempt := 0; attempt < temporaryAttempts; attempt++ {
		previous := sequence.Load()
		if previous == ^uint64(0) {
			return 0, false
		}
		if sequence.CompareAndSwap(previous, previous+1) {
			return previous + 1, true
		}
	}
	return 0, false
}

func reserveTemporary(files Files, target []byte, temp *[limits.PathMax]byte, sequence *atomic.Uint64) ([]byte, vfs.Status) {
	prefix, status := TemporaryPath(target, temp)
	if status != vfs.StatusOK {
		return nil, status
	}
	nameStart := len(prefix) - 1
	for prefix[nameStart] != '/' {
		nameStart--
	}
	if len(prefix)+temporaryIDBytes > limits.PathMax || len(prefix)-nameStart-1+temporaryIDBytes > limits.NameMax {
		return nil, vfs.StatusNameTooLong
	}
	path := temp[:len(prefix)+temporaryIDBytes]
	path[len(prefix)] = '.'
	const hex = "0123456789abcdef"
	for attempt := 0; attempt < temporaryAttempts; attempt++ {
		id, available := nextTemporaryID(sequence)
		if !available {
			return nil, vfs.StatusBusy
		}
		for digit := len(path) - 1; digit > len(prefix); digit-- {
			path[digit] = hex[id&15]
			id >>= 4
		}
		status = files.Create(path)
		if status == vfs.StatusAlreadyExists {
			continue
		}
		return path, status
	}
	return nil, vfs.StatusBusy
}

// Save writes what source produces beside target, through buffer, and then
// atomically replaces target. It reserves a request-owned candidate name by
// exclusive Create; existing candidates (including stale files) are never
// opened, removed, or changed. They remain for explicit owner cleanup. After
// bounded collisions, counter exhaustion, or contention it reports StatusBusy
// at StageTemporary without consuming source.
//
// Before Replace, failures remove only this save's file best effort and leave
// target unchanged. A Replace error may follow a committed rename, so neither
// pathname is touched afterward and the caller must reconcile the result. A
// retry uses another candidate even if an earlier file remains. Concurrent
// callers need separate buffer, source, and temp workspaces and must not modify
// another save's file.
func Save(files Files, target []byte, buffer []byte, source Source, temp *[limits.PathMax]byte) (vfs.Status, Stage) {
	return saveWithSequence(files, target, buffer, source, temp, &temporarySequence)
}

func saveWithSequence(files Files, target []byte, buffer []byte, source Source, temp *[limits.PathMax]byte, sequence *atomic.Uint64) (vfs.Status, Stage) {
	if len(buffer) == 0 {
		return vfs.StatusInvalidArgument, StageWrite
	}
	path, status := reserveTemporary(files, target, temp, sequence)
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
		// Replace may already have moved the file. Another process can then
		// own path, so path-only cleanup could delete its temporary file.
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
