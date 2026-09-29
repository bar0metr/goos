package filemanager

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/shell"
)

const (
	// MaxEntries bounds the entries one panel holds, and namePoolSize the
	// bytes of their names. A directory with more is shown truncated, and
	// says so, rather than cut silently.
	MaxEntries   = 1024
	namePoolSize = 64 << 10
)

// Entry is one listed name. Its name lives in the panel's pool.
type Entry struct {
	nameOffset uint32
	nameSize   uint16
	Kind       ramfs.Kind
	Size       uint64
	Modified   ramfs.Timestamp
}

var parentName = []byte("..")

// Panel is one side of the manager: a directory, its sorted listing, and the
// selection and scroll position within it. It allocates nothing.
type Panel struct {
	path      [shell.MaxPath]byte
	pathSize  int
	entries   [MaxEntries]Entry
	count     int
	names     [namePoolSize]byte
	namesSize int
	// truncated is set when the directory had more entries than fit.
	truncated bool
	selected  int
	top       int
	// filesystem describes the filesystem the directory is on, as of the last
	// listing; it is asked again only when the listing is, never per key.
	filesystem ramfs.FilesystemInfo
	mount      vfs.MountID
	described  bool
}

// Path returns the directory the panel shows.
func (panel *Panel) Path() []byte { return panel.path[:panel.pathSize] }

// SetPath changes the directory; the listing is loaded separately.
func (panel *Panel) SetPath(path []byte) bool {
	if len(path) == 0 || len(path) > len(panel.path) || path[0] != '/' {
		return false
	}
	panel.pathSize = copy(panel.path[:], path)
	return true
}

// Count returns how many entries are listed, ".." included.
func (panel *Panel) Count() int { return panel.count }

// Truncated reports whether the directory had entries that did not fit.
func (panel *Panel) Truncated() bool { return panel.truncated }

// Selected returns the index of the selected entry.
func (panel *Panel) Selected() int { return panel.selected }

// Top returns the index of the first visible entry.
func (panel *Panel) Top() int { return panel.top }

// Entry returns entry index and its name.
func (panel *Panel) Entry(index int) (Entry, []byte) {
	entry := panel.entries[index]
	return entry, panel.names[entry.nameOffset : int(entry.nameOffset)+int(entry.nameSize)]
}

// Current returns the selected entry, or false for an empty listing.
func (panel *Panel) Current() (Entry, []byte, bool) {
	if panel.count == 0 {
		return Entry{}, nil, false
	}
	entry, name := panel.Entry(panel.selected)
	return entry, name, true
}

// IsParent reports whether name is the ".." entry.
func IsParent(name []byte) bool { return len(name) == 2 && name[0] == '.' && name[1] == '.' }

// Visit adds one entry to the listing.
func (panel *Panel) Visit(name []byte, kind ramfs.Kind, size uint64, modified ramfs.Timestamp) bool {
	if panel.count == len(panel.entries) || len(name) > len(panel.names)-panel.namesSize || len(name) > 0xffff {
		panel.truncated = true
		return false
	}
	panel.entries[panel.count] = Entry{nameOffset: uint32(panel.namesSize), nameSize: uint16(len(name)), Kind: kind, Size: size, Modified: modified}
	panel.namesSize += copy(panel.names[panel.namesSize:], name)
	panel.count++
	return true
}

// Load lists the panel's directory. The entry named keep is selected if it is
// still there; otherwise the selection keeps its position. On failure the
// listing holds only "..", so the panel can still be left.
func (panel *Panel) Load(filesystem FileSystem, keep []byte, visible int) vfs.Status {
	panel.count, panel.namesSize, panel.truncated = 0, 0, false
	if panel.pathSize > 1 {
		panel.Visit(parentName, ramfs.KindDirectory, 0, ramfs.Timestamp{})
	}
	first := panel.count
	status := filesystem.ReadDir(panel.Path(), panel)
	if status != vfs.StatusOK {
		panel.count, panel.namesSize = first, len(parentName)*first
	}
	sortEntries(panel, first)
	if keep != nil {
		for index := 0; index < panel.count; index++ {
			if _, name := panel.Entry(index); equalBytes(name, keep) {
				panel.selected = index
				break
			}
		}
	}
	panel.Select(panel.selected, visible)
	panel.filesystem, panel.mount, panel.described = ramfs.FilesystemInfo{}, 0, false
	if status == vfs.StatusOK {
		info, mount, infoStatus := filesystem.FilesystemInfo(panel.Path())
		panel.filesystem, panel.mount, panel.described = info, mount, infoStatus == vfs.StatusOK
	}
	return status
}

// Filesystem returns what the last listing learned about the filesystem the
// directory is on, and false when it could not be asked.
func (panel *Panel) Filesystem() (ramfs.FilesystemInfo, vfs.MountID, bool) {
	return panel.filesystem, panel.mount, panel.described
}

// Select moves the selection to index, kept within the listing, and scrolls
// so it is among the visible rows.
func (panel *Panel) Select(index, visible int) {
	if index >= panel.count {
		index = panel.count - 1
	}
	if index < 0 {
		index = 0
	}
	panel.selected = index
	if visible < 1 {
		visible = 1
	}
	if panel.top > panel.selected {
		panel.top = panel.selected
	}
	if panel.selected >= panel.top+visible {
		panel.top = panel.selected - visible + 1
	}
	if panel.top > panel.count-visible {
		panel.top = panel.count - visible
	}
	if panel.top < 0 {
		panel.top = 0
	}
}

// Move moves the selection by delta entries.
func (panel *Panel) Move(delta, visible int) { panel.Select(panel.selected+delta, visible) }

// sortEntries orders the entries from first on: directories before files,
// then by name with ASCII letters compared case-insensitively, then by bytes
// so the order is total and UTF-8 names sort deterministically. It is a
// shell sort: in place, bounded, and without recursion.
func sortEntries(panel *Panel, first int) {
	entries := panel.entries[first:panel.count]
	for gap := len(entries) / 2; gap > 0; gap /= 2 {
		for index := gap; index < len(entries); index++ {
			moving := entries[index]
			position := index
			for position >= gap && panel.less(moving, entries[position-gap]) {
				entries[position] = entries[position-gap]
				position -= gap
			}
			entries[position] = moving
		}
	}
}

func (panel *Panel) less(left, right Entry) bool {
	leftDirectory, rightDirectory := left.Kind == ramfs.KindDirectory, right.Kind == ramfs.KindDirectory
	if leftDirectory != rightDirectory {
		return leftDirectory
	}
	leftName := panel.names[left.nameOffset : int(left.nameOffset)+int(left.nameSize)]
	rightName := panel.names[right.nameOffset : int(right.nameOffset)+int(right.nameSize)]
	if order := compareNames(leftName, rightName, true); order != 0 {
		return order < 0
	}
	return compareNames(leftName, rightName, false) < 0
}

func compareNames(left, right []byte, fold bool) int {
	for index := 0; index < len(left) && index < len(right); index++ {
		a, b := left[index], right[index]
		if fold {
			a, b = lower(a), lower(b)
		}
		if a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	return len(left) - len(right)
}

func lower(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + 'a' - 'A'
	}
	return value
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
