package config

import (
	"goos/internal/limits"
	"goos/kernel/vfs"
	"goos/userspace/safesave"
)

// PersistStore writes a bounded typed export beside target, closes it, and
// acknowledges replacement. It does not promise power-loss atomicity on FAT.
func PersistStore(files safesave.Files, target []byte, store *Store, buffer []byte, temporary *[limits.PathMax]byte) (vfs.Status, safesave.Stage) {
	scope, _ := MakeScope("")
	source := exportSource{store: store, scope: scope}
	return safesave.Save(files, target, buffer, &source, temporary)
}

type exportSource struct {
	store        *Store
	scope        Scope
	cursor       int
	line         [MaxKey + MaxText + 16]byte
	size, offset int
}

func (source *exportSource) Fill(buffer []byte) int {
	if source.offset < source.size {
		count := copy(buffer, source.line[source.offset:source.size])
		source.offset += count
		return count
	}
	for {
		key, value, readOnly, next, status := source.store.At(source.scope, Read, source.cursor)
		if status != StatusOK {
			return 0
		}
		source.cursor = next
		if readOnly {
			continue
		}
		line, ok := AppendEntry(source.line[:0], key, value)
		if !ok {
			return 0
		}
		source.size = len(line)
		source.offset = copy(buffer, line)
		return source.offset
	}
}

// PersistenceStatus distinguishes pre-replacement failure from an I/O error
// that may follow a completed replacement. Unknown requires forward retry.
func PersistenceStatus(status vfs.Status, stage safesave.Stage) Status {
	if status == vfs.StatusOK {
		return StatusOK
	}
	if stage == safesave.StageReplace {
		switch status {
		case vfs.StatusBackendError, vfs.StatusTimeout, vfs.StatusPeerDied, vfs.StatusCorrupt, vfs.StatusNoSpace:
			return StatusPersistUnknown
		}
	}
	return StatusPersistFailed
}
