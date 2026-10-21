package exec

import "goos/kernel/initramfs"

// BootstrapRoot is where the boot archive is mounted. PID 1 launches a program
// under it from the archive by extent; every other path names a file, read
// through the VFS into an executable object (ADR 0097, ADR 0175). The archive
// keeps its members under archivePrefix, which the kernel reads system/init
// from, so the member of /bootstrap/bin/ls is system/bin/ls.
const (
	BootstrapRoot = "/bootstrap/"
	archivePrefix = "system/"
)

// Origin says where an executable's bytes come from.
type Origin uint8

const (
	// OriginFile is a file of the namespace: the installed system, an
	// application, or anything a user put on the volume.
	OriginFile Origin = iota
	// OriginArchive is a member of the boot archive.
	OriginArchive
)

// OriginOf returns the origin of a canonical absolute path. The archive is
// chosen only for a path under BootstrapRoot, and nothing else ever reads it:
// a file missing from the volume is not looked for in the archive.
func OriginOf(path []byte) Origin {
	if InBootstrap(path) {
		return OriginArchive
	}
	return OriginFile
}

// ImageStatus says why a path does not name an executable in the archive.
type ImageStatus uint8

const (
	ImageFound ImageStatus = iota
	// ImageOutsideArchive is a canonical path that is not under
	// BootstrapRoot. The archive does not hold it; the filesystem may.
	ImageOutsideArchive
	// ImageNotFound is a canonical path under BootstrapRoot the archive
	// lacks. It is not looked up anywhere else.
	ImageNotFound
	// ImageTooLarge is a member whose extent cannot be named in a launch.
	ImageTooLarge
	// ImageInvalidPath is a path that is not absolute and canonical.
	ImageInvalidPath
)

// LocateImage resolves an absolute executable path in the mapped boot archive
// and returns the extent of its bytes there. This is the executable search the
// kernel used to do on every launch (ADR 0093): the path, its canonical form,
// the mount point, and the archive format are PID 1's policy, and the kernel
// receives only the extent and a capability to execute from the archive.
//
// The path must be canonical: absolute, with no empty, "." or ".."
// components. Hard links resolve to the member they name, so every name of one
// program yields the same extent and shares its read-only pages.
func LocateImage(image []byte, path []byte) (uint32, uint32, ImageStatus) {
	if !validPath(path) {
		return 0, 0, ImageInvalidPath
	}
	if !InBootstrap(path) {
		return 0, 0, ImageOutsideArchive
	}
	offset, size, found := ArchiveExtent(image, path)
	if !found {
		return 0, 0, ImageNotFound
	}
	if size == 0 || uint64(offset) > 1<<32-1 || uint64(size) > 1<<32-1 {
		return 0, 0, ImageTooLarge
	}
	return uint32(offset), uint32(size), ImageFound
}

// ArchiveExtent finds the member of a path below BootstrapRoot in the mapped
// boot archive and returns the offset and length of its bytes. It joins
// archivePrefix and the rest of the path only while comparing names, so it
// keeps no buffer that two callers could share.
func ArchiveExtent(image []byte, path []byte) (int, int, bool) {
	if len(path) <= len(BootstrapRoot) || !hasPrefix(path, BootstrapRoot) {
		return 0, 0, false
	}
	return initramfs.ExtentUnder(image, []byte(archivePrefix), path[len(BootstrapRoot):])
}

// InBootstrap reports whether a canonical path is /bootstrap or lies under it.
func InBootstrap(path []byte) bool {
	root := BootstrapRoot[:len(BootstrapRoot)-1]
	return component(path, root) || len(path) > len(BootstrapRoot) && hasPrefix(path, BootstrapRoot)
}

func hasPrefix(path []byte, prefix string) bool {
	return len(path) >= len(prefix) && component(path[:len(prefix)], prefix)
}

// InSystem reports whether a canonical path is /system or lies under it: the
// installed system, which holds services and commands but whose services are
// never commands.
func InSystem(path []byte) bool {
	root := SystemRoot[:len(SystemRoot)-1]
	return component(path, root) || len(path) > len(SystemRoot) && hasPrefix(path, SystemRoot)
}

// SystemRoot is where the installed system of the volume is published.
const SystemRoot = "/system/"
