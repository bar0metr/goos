// Package filemanager is gfm, the GoOS two-pane file manager, without its
// process plumbing: the panels, the keys, the dialogs and the screen it
// draws. It reaches files only through the FileSystem it is given, which the
// command backs with its VFS session, and the terminal only through the bytes
// it renders, so it runs the same on the framebuffer and on COM1 and can be
// tested on the host (ADR 0124).
package filemanager

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

// Visitor receives the entries of a directory listing.
type Visitor interface {
	// Visit is given one entry; the name is only valid during the call. It
	// returns false to end the listing early.
	Visit(name []byte, kind ramfs.Kind, size uint64) bool
}

// FileSystem is what the manager does to files. Every operation reports its
// status instead of ending the program: a failed one is shown and the manager
// carries on.
type FileSystem interface {
	ReadDir(path []byte, visitor Visitor) vfs.Status
	Stat(path []byte) (ramfs.Kind, uint64, vfs.Status)
	Mkdir(path []byte) vfs.Status
	Rename(source, target []byte) vfs.Status
	Unlink(path []byte) vfs.Status
	// CopyFile copies a regular file, replacing an existing target only when
	// overwrite is set. targetFailed says which side a failure was on.
	CopyFile(source, target []byte, overwrite bool) (status vfs.Status, targetFailed bool)
}
