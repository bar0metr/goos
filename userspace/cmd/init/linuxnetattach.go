//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// linuxNetAttach hands a running Linux launch a new family session through
// the kernel: the kernel replaces the family's linux-net and linux-net-pool
// with channel and pool, both consumed on success. It needs PID 1's handle
// to the launch's root process.
func linuxNetAttach(process, channel, pool handle.ID) bool {
	return sys.LinuxNetAttach(process, channel, pool) == kcall.StatusOK
}
