package goroutine

// readyState records one wake credit. Only the CAS that wakes a parked
// stack owns a new enqueue; runnable/running stacks already have an owner.
func (g *G) readyState() bool {
	for {
		state := g.status.Load()
		var next uint32
		switch state {
		case gParked:
			next = gRunnable
		case gRunning:
			next = gWaking
		case gRunnable:
			next = gRunnableWaking
		default:
			return false
		}
		if g.status.CompareAndSwap(state, next) {
			return state == gParked
		}
	}
}

// dispatchState and rescheduleState preserve an unconsumed wake. A task may
// publish its waiter before Pause, then be preempted before that call.
func (g *G) dispatchState() {
	for {
		state := g.status.Load()
		next := uint32(gRunning)
		switch state {
		case gRunnable:
		case gRunnableWaking:
			next = gWaking
		default:
			panic("goroutine: dispatch of a non-runnable goroutine")
		}
		if g.status.CompareAndSwap(state, next) {
			return
		}
	}
}

func (g *G) rescheduleState() {
	for {
		state := g.status.Load()
		next := uint32(gRunnable)
		switch state {
		case gRunning:
		case gWaking:
			next = gRunnableWaking
		default:
			panic("goroutine: reschedule of a non-running goroutine")
		}
		if g.status.CompareAndSwap(state, next) {
			return
		}
	}
}

// parkState runs only after the stack has been saved. It consumes an early
// wake, returning true when the scheduler must enqueue the stack again.
// If a concurrent ready wins after gParked, that caller owns the enqueue.
func (g *G) parkState() bool {
	for {
		state := g.status.Load()
		next := uint32(gParked)
		switch state {
		case gRunning:
		case gWaking:
			next = gRunnable
		default:
			panic("goroutine: park of a non-running goroutine")
		}
		if g.status.CompareAndSwap(state, next) {
			return state == gWaking
		}
	}
}
