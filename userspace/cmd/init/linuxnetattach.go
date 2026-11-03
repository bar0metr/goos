//go:build goos_userspace

package main

import "goos/kernel/handle"

// linuxNetAttach hands a running Linux launch a new family session through
// the kernel: the kernel replaces the family's linux-net and linux-net-pool
// with channel and pool, both consumed on success. It needs PID 1's handle
// to the launch's root process.
//
// The kernel call is sys.LinuxNetAttach(process, channel, pool). Until the
// kernel side lands this reports failure, and a launch that outlives a
// linuxsockd restart keeps running without sockets.
func linuxNetAttach(process, channel, pool handle.ID) bool {
	_, _, _ = process, channel, pool
	return false
}
