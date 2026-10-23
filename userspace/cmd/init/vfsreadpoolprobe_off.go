//go:build goos_userspace && !goos_vfs_probe

package main

// probeVFSReadPool is absent from ordinary and ordinary validation images.
func probeVFSReadPool() bool { return true }
