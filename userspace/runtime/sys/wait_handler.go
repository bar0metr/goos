package sys

import (
	"sync/atomic"

	"goos/kernel/handle"
	"goos/kernel/kcall"
)

// WaitHandler lets the goroutine scheduler park a caller without blocking
// its native thread. A handler returns handled=false on scheduler stacks,
// where WaitAny must use the kernel. Polls bypass the handler.
// The runtime installs it before running application goroutines (ADR 0204).
type WaitHandler func(*[kcall.WaitAnyMaxObjects]Interest, int, uint64) (mask uint64, status kcall.Status, handled bool)

var waitHandler atomic.Pointer[WaitHandler]

// InstallWaitHandler publishes a runtime-owned handler, or nil to use native
// waits. The caller must keep the function value immutable after publishing.
func InstallWaitHandler(handler *WaitHandler) { waitHandler.Store(handler) }

// ReceiveHandler parks an IPC receiver through the scheduler. It follows the
// same native fallback contract as WaitHandler and must not retain buffer.
type ReceiveHandler func(channel handle.ID, buffer []byte, timeout uint64) (status kcall.Status, handled bool)

var receiveHandler atomic.Pointer[ReceiveHandler]

// InstallReceiveHandler publishes the runtime's IPC adapter before user code.
func InstallReceiveHandler(handler *ReceiveHandler) { receiveHandler.Store(handler) }

// ProcessWaitHandler parks a process waiter through the scheduler. It runs
// before ProcessWait pins its native scratch and follows WaitHandler's native
// fallback contract. Zero-time probes bypass it.
type ProcessWaitHandler func(process handle.ID, timeout uint64) (code uint64, status kcall.Status, handled bool)

var processWaitHandler atomic.Pointer[ProcessWaitHandler]

// InstallProcessWaitHandler publishes an immutable runtime-owned handler,
// or nil to use native process waits.
func InstallProcessWaitHandler(handler *ProcessWaitHandler) { processWaitHandler.Store(handler) }

// ThreadJoinHandler parks a thread joiner through the scheduler. It runs
// before ThreadJoin pins its native scratch and follows WaitHandler's native
// fallback contract. Polls bypass it.
type ThreadJoinHandler func(thread uint64) (code uint64, status kcall.Status, handled bool)

var threadJoinHandler atomic.Pointer[ThreadJoinHandler]

// InstallThreadJoinHandler publishes an immutable runtime-owned handler, or
// nil to use native joins.
func InstallThreadJoinHandler(handler *ThreadJoinHandler) { threadJoinHandler.Store(handler) }
