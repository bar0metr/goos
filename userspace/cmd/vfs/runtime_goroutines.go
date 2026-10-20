//go:build goos_userspace && goos_goroutines

package main

import (
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/goroutine"
)

func initRuntime() {
	// Adopt main and run package initializers first. Preserve the service's
	// image prefaulting and heap warmup before creating application workers.
	goroutine.Init()
	arena.InitService()
}
