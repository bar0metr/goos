//go:build goos_userspace && goos_goroutines

package main

import (
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/goroutine"
)

func initRuntime() {
	// Adopt main and run package initializers first, then warm the service
	// heap before any read worker starts (ADR 0204, ADR 0232).
	goroutine.Init()
	arena.InitService()
}
