// Package goroutine is the GoOS scheduler for Go goroutines in native
// programs (ADR 0204). A program built with the goroutine toolchain
// (tinygo/targets/goos-userspace-goroutines-amd64.json) runs go statements,
// channels, select, the sync package, timers, and time.Sleep on it.
//
// Goroutines (G) run on native threads of the process (M, ADR 0203). A
// thread runs Go code on two kinds of stack: a goroutine's, and its own
// scheduler stack, where it picks the next goroutine. At most GOMAXPROCS
// threads run goroutines at once; each holds a permit for it (the P of
// other Go runtimes) and a bounded queue of runnable goroutines that idle
// threads steal from. The main function runs as a goroutine on the main
// thread's stack, and a function thread.Start runs stays on its thread.
//
// A goroutine switches out only where the runtime is consistent: when it
// blocks on a channel, a lock, a timer, or a handle, when it yields, and
// when the kernel diverts its thread to the preemption entry after it has
// run for a while and the thread is not pinned (tls.Pin). A thread pins
// itself while it uses state of its own: system-call descriptors,
// allocator caches, and the scheduler's own structures.
//
// Blocking native calls block their thread. Wait parks only the goroutine:
// an idle thread waits on the kernel for every handle goroutines wait on,
// and for the next timer.
package goroutine
