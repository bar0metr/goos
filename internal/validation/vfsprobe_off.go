//go:build !goos_vfs_probe

package validation

// VFSReadPoolProbe reports whether this validation image carries the explicit
// native goroutine VFS acceptance fixture.
const VFSReadPoolProbe = false
