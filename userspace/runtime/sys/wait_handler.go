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
