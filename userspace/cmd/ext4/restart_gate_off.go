//go:build goos_userspace && goos_goroutines && (!goos_validation || !goos_ext4_restart_gate)

package main

import "goos/userspace/fsproto"

func restartGateRead(request fsproto.Request) bool { return false }
func holdRestartRead() bool                        { return false }
