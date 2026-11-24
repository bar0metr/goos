//go:build goos_userspace && !goos_goroutines

package main

import "goos/userspace/runtime/arena"

func initRuntime() {
	arena.InitService()
}
