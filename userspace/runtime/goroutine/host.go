//go:build !(goos_userspace && goos_goroutines)

package goroutine

import "runtime"

// A host build, or a native one without the goroutine runtime, has nothing
// to pin and gives up its processor through the Go runtime.

func pin()         {}
func unpin()       {}
func threadYield() { runtime.Gosched() }
