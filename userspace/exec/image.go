package exec

import "goos/kernel/initramfs"

// SystemRoot is where the immutable system image is mounted. Services and the
// commands in /system/bin live under it; a program anywhere else is read from
// the filesystem into an executable object (ADR 0097).
const SystemRoot = "/system/"

// ImageStatus says why a path does not name an executable in the image.
type ImageStatus uint8

const (
	ImageFound ImageStatus = iota
	// ImageOutsideSystem is a canonical path that is not under SystemRoot.
	// The system image does not hold it; the filesystem may.
	ImageOutsideSystem
	// ImageNotFound is a canonical path under SystemRoot the image lacks. It
	// is not looked up anywhere else: /system is the image and nothing more.
	ImageNotFound
	// ImageTooLarge is a member whose extent cannot be named in a launch.
	ImageTooLarge
	// ImageInvalidPath is a path that is not absolute and canonical.
	ImageInvalidPath
)

// LocateImage resolves an absolute executable path in the mapped system image
// and returns the extent of its bytes there. This is the executable search the
// kernel used to do on every launch (ADR 0093): the path, its canonical form,
// the mount point, and the archive format are PID 1's policy, and the kernel
// receives only the extent and a capability to execute from the image.
//
// The path must be canonical: absolute, with no empty, "." or ".."
// components. Hard links resolve to the member they name, so every name of one
// program yields the same extent and shares its read-only pages.
func LocateImage(image []byte, path []byte) (uint32, uint32, ImageStatus) {
	if !validPath(path) {
		return 0, 0, ImageInvalidPath
	}
	if !InSystem(path) {
		return 0, 0, ImageOutsideSystem
	}
	if len(path) <= len(SystemRoot) {
		return 0, 0, ImageNotFound
	}
	// Archive members carry their whole path without the leading slash, so
	// "/system/vfs" is the member "system/vfs".
	offset, size, found := initramfs.Extent(image, path[1:])
	if !found {
		return 0, 0, ImageNotFound
	}
	if size == 0 || uint64(offset) > 1<<32-1 || uint64(size) > 1<<32-1 {
		return 0, 0, ImageTooLarge
	}
	return uint32(offset), uint32(size), ImageFound
}

// InSystem reports whether a canonical path is /system or lies under it, where
// only the system image is looked in.
func InSystem(path []byte) bool {
	root := SystemRoot[:len(SystemRoot)-1]
	return component(path, root) || len(path) > len(SystemRoot) && component(path[:len(SystemRoot)], SystemRoot)
}
