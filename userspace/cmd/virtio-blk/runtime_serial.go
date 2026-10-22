//go:build goos_userspace && !goos_goroutines

package main

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
)

func initRuntime() { arena.InitService() }

func irqWait(irq handle.ID) kcall.Status { return sys.IRQWait(irq) }
