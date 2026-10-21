//go:build goos_userspace && !goos_goroutines

package main

import "goos/kernel/vfs"

func readPoolProbeOpen(vfs.Owner, vfs.Request, vfs.Reply) {}
func readPoolProbeSessionClosed(vfs.Owner)                {}
